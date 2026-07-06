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
	"time"
)

// Handler is the daemon-side brain behind the control socket. HandleRequest
// runs on the connection's goroutine — an attach that waits out an in-flight
// wake turn blocks only its own connection, which is exactly what the waiting
// client wants. ConnClosed fires exactly once per connection after its read
// loop ends (including on server Close), letting the runtime resume after an
// attach connection drops.
type Handler interface {
	HandleRequest(ctx context.Context, req Request, conn *ServerConn) Response
	ConnClosed(conn *ServerConn)
}

// ServerConn identifies one accepted connection. The attached marker is owned
// by the handler (only the runtime knows whether an attach succeeded); it
// lives here so ConnClosed can cheaply distinguish an attach lease ending from
// a one-shot status probe hanging up.
type ServerConn struct {
	id       uint64
	conn     net.Conn
	writeMu  sync.Mutex
	attached atomic.Bool
}

// ID returns the connection's server-unique id.
func (c *ServerConn) ID() uint64 { return c.id }

// SetAttached marks/unmarks this connection as holding the attach lease.
func (c *ServerConn) SetAttached(v bool) { c.attached.Store(v) }

// Attached reports whether this connection holds the attach lease.
func (c *ServerConn) Attached() bool { return c.attached.Load() }

// writeResponse writes one NDJSON response frame. Serialized per-connection so
// a slow client can't interleave two frames.
func (c *ServerConn) writeResponse(resp Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if len(data)+1 > maxFrameBytes {
		return fmt.Errorf("ipc: oversize response frame (%d bytes)", len(data))
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = c.conn.Write(append(data, '\n'))
	return err
}

// Server owns the unix control socket: bind, accept loop, per-connection read
// loops. It does not know anything about supervision — that's the Handler.
type Server struct {
	path    string
	handler Handler

	mu     sync.Mutex
	ln     net.Listener
	conns  map[uint64]*ServerConn
	nextID uint64
	closed bool
	wg     sync.WaitGroup
}

// NewServer prepares a server for the given socket path.
func NewServer(path string, h Handler) *Server {
	return &Server{path: path, handler: h, conns: make(map[uint64]*ServerConn)}
}

// Start binds the socket and launches the accept loop. A stale socket file
// (left by a crashed daemon) is unlinked iff nothing answers a probe dial —
// safe because the caller holds the daemon singleton flock, so no OTHER daemon
// can be mid-bind on the same path.
func (s *Server) Start(ctx context.Context) error {
	if _, err := os.Stat(s.path); err == nil {
		probe, derr := net.DialTimeout("unix", s.path, 500*time.Millisecond)
		if derr == nil {
			_ = probe.Close()
			return fmt.Errorf("ipc: socket %s is already served by a live process", s.path)
		}
		_ = os.Remove(s.path)
	}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("ipc: bind %s: %w", s.path, err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("ipc: chmod %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.wg.Add(1)
	go s.acceptLoop(ctx)
	return nil
}

func (s *Server) acceptLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			// Accept fails permanently once the listener closes; anything else on a
			// unix listener is equally terminal. Either way the server is done.
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.nextID++
		sc := &ServerConn{id: s.nextID, conn: conn}
		s.conns[sc.id] = sc
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serveConn(ctx, sc)
	}
}

func (s *Server) serveConn(ctx context.Context, sc *ServerConn) {
	defer s.wg.Done()
	defer func() {
		_ = sc.conn.Close()
		s.mu.Lock()
		delete(s.conns, sc.id)
		s.mu.Unlock()
		// After the read loop: exactly-once by construction (serveConn runs once
		// per conn). The handler resumes supervision here when an attach lease ends.
		s.handler.ConnClosed(sc)
	}()
	scanner := bufio.NewScanner(sc.conn)
	scanner.Buffer(make([]byte, 0, 16*1024), maxFrameBytes)
	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = sc.writeResponse(Response{V: ProtocolVersion, OK: false, Error: "malformed request frame"})
			continue
		}
		if req.V != ProtocolVersion {
			_ = sc.writeResponse(Response{V: ProtocolVersion, ID: req.ID, OK: false,
				Error: fmt.Sprintf("protocol version mismatch (daemon %d, client %d) — restart the daemon", ProtocolVersion, req.V)})
			continue
		}
		resp := s.handler.HandleRequest(ctx, req, sc)
		resp.V = ProtocolVersion
		resp.ID = req.ID
		if err := sc.writeResponse(resp); err != nil {
			return
		}
	}
}

// Close shuts the listener and all live connections, then waits for the loops
// to drain (each firing its ConnClosed). Idempotent.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.wg.Wait()
		return
	}
	s.closed = true
	ln := s.ln
	conns := make([]*ServerConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.conn.Close()
	}
	s.wg.Wait()
	_ = os.Remove(s.path)
}

// ErrNoDaemon distinguishes "nothing is listening" from a real transport
// failure, so callers can decide to spawn a daemon instead of surfacing an
// error.
var ErrNoDaemon = errors.New("ipc: no daemon is listening on the control socket")
