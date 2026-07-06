package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Client is one connection to the daemon control socket. Calls are serialized
// (one in flight at a time) — the protocol is strict request/response per
// connection, so there is nothing to multiplex. A Client held open after a
// successful attach IS the attach lease: Close() (or process death) releases
// it and the daemon resumes.
type Client struct {
	conn   net.Conn
	reader *bufio.Scanner
	mu     sync.Mutex
	nextID atomic.Uint64
	// broken latches after any read failure: bufio.Scanner does not resume
	// cleanly after an I/O timeout mid-line, so a timed-out Call poisons the
	// stream — every later Call must fail fast and the caller reconnect.
	broken bool
}

// Dial connects to the control socket. A missing socket file or a
// connection-refused (stale socket, daemon gone) maps to ErrNoDaemon so
// callers can branch to "spawn one".
func Dial(path string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		if isNoDaemonDialError(err) {
			return nil, ErrNoDaemon
		}
		return nil, fmt.Errorf("ipc: dial %s: %w", path, err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 16*1024), maxFrameBytes)
	return &Client{conn: conn, reader: sc}, nil
}

func isNoDaemonDialError(err error) bool {
	// Absent socket file, a socket nobody accepts on (crashed daemon), or a
	// non-socket file squatting on the path — all mean "no live daemon".
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOTSOCK)
}

// Call sends one request and decodes the matching response payload into out
// (out may be nil). The ctx deadline bounds the whole round trip; ReqAttach
// callers should allow generous time — the daemon may be finishing an
// in-flight wake turn before it acks.
func (c *Client) Call(ctx context.Context, typ string, payload any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken {
		return fmt.Errorf("ipc: connection is broken after an earlier read failure — reconnect")
	}
	id := fmt.Sprintf("r%d", c.nextID.Add(1))
	req := Request{V: ProtocolVersion, ID: id, Type: typ}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("ipc: marshal %s payload: %w", typ, err)
		}
		req.Payload = raw
	}
	frame, err := json.Marshal(req)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)
	if _, err := c.conn.Write(append(frame, '\n')); err != nil {
		return fmt.Errorf("ipc: write %s: %w", typ, err)
	}
	for c.reader.Scan() {
		var resp Response
		if err := json.Unmarshal(c.reader.Bytes(), &resp); err != nil {
			return fmt.Errorf("ipc: malformed response frame: %w", err)
		}
		if resp.ID != id {
			// Stale frame from a previous timed-out call; skip until ours.
			continue
		}
		if !resp.OK {
			return fmt.Errorf("ipc: %s: %s", typ, resp.Error)
		}
		if out != nil && len(resp.Payload) > 0 {
			if err := json.Unmarshal(resp.Payload, out); err != nil {
				return fmt.Errorf("ipc: decode %s reply: %w", typ, err)
			}
		}
		return nil
	}
	c.broken = true
	if err := c.reader.Err(); err != nil {
		return fmt.Errorf("ipc: read %s reply: %w", typ, err)
	}
	return fmt.Errorf("ipc: connection closed before %s reply", typ)
}

// Close hangs up. For an attached client this releases the attach lease — the
// daemon's ConnClosed fires and it resumes contending for ownership.
func (c *Client) Close() error {
	return c.conn.Close()
}
