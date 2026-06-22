#!/usr/bin/env python3
"""
Drive the cockpit harness under a real PTY and analyse the ACTUAL terminal byte
stream — the thing OpenTUI's testRender harness can't show.

Spawns `bun tests/ui/e2e/harness.tsx` on a pseudo-terminal sized 80x24 in HOLD mode
(the harness seals the turn, prints E2E_SEALED to stderr, then stays alive WITHOUT
tearing the renderer down so the captured stream ends at peak content). The runner
snapshots, kills it, and reports:

  * footer_repaints — count of split-footer FULL repaints. OpenTUI repaints the footer
    by homing the cursor to the footer's top line and rewriting it; a resize-driven
    flash shows up as a burst of cursor-home + full-region rewrites. We approximate the
    flash with the scroll + full-region-clear signals below.
  * scroll_up / scroll_down — CSI <n> S / CSI <n> T (footer-resize viewport scrolls).
  * csi_hist — histogram of CSI final bytes, so we can SEE what the renderer emits.
  * a pyte reconstruction of the peak visible screen (response + tools + composer).

JSON report to stdout. Optional argv[1] = path to also dump the raw byte stream.
"""
import json
import os
import pty
import re
import select
import struct
import sys
import termios
import fcntl
import subprocess
import time

ROWS = int(os.environ.get("DAINTREE_E2E_ROWS", "24"))
COLS = int(os.environ.get("DAINTREE_E2E_COLS", "80"))
TIMEOUT_S = 25.0

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))

RE_SCROLL_UP = re.compile(rb"\x1b\[(\d*)S")
RE_SCROLL_DOWN = re.compile(rb"\x1b\[(\d*)T")
RE_CLEAR_SCREEN = re.compile(rb"\x1b\[[0-3]?J")
# Any CSI sequence: ESC [ <params> <final byte 0x40-0x7e>
RE_CSI = re.compile(rb"\x1b\[[0-9;?]*([@-~])")


def run() -> dict:
    master, slave = pty.openpty()
    winsize = struct.pack("HHHH", ROWS, COLS, 0, 0)
    fcntl.ioctl(slave, termios.TIOCSWINSZ, winsize)

    env = dict(os.environ)
    env.setdefault("TERM", "xterm-256color")
    env["DAINTREE_E2E_STEP_MS"] = env.get("DAINTREE_E2E_STEP_MS", "40")
    env["DAINTREE_E2E_HOLD"] = "1"
    env["DAINTREE_ASSISTANT_DEBUG_LOG"] = "0"

    proc = subprocess.Popen(
        ["bun", "tests/ui/e2e/harness.tsx"],
        cwd=REPO,
        env=env,
        stdin=slave,
        stdout=slave,
        stderr=subprocess.PIPE,
        close_fds=True,
    )
    os.close(slave)
    os.set_blocking(proc.stderr.fileno(), False)

    chunks = bytearray()
    stderr_buf = bytearray()
    sealed_at = None
    deadline = time.time() + TIMEOUT_S
    while True:
        now = time.time()
        if now > deadline:
            break
        # Once sealed, grab a short grace period of trailing output then stop.
        if sealed_at is not None and now > sealed_at + 0.4:
            break
        try:
            r, _, _ = select.select([master, proc.stderr], [], [], 0.1)
        except (OSError, ValueError):
            break
        if proc.stderr in r:
            try:
                e = os.read(proc.stderr.fileno(), 65536)
                if e:
                    stderr_buf.extend(e)
                    if b"E2E_SEALED" in stderr_buf and sealed_at is None:
                        sealed_at = time.time()
            except OSError:
                pass
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            chunks.extend(data)

    proc.kill()
    try:
        os.close(master)
    except OSError:
        pass
    try:
        proc.wait(timeout=2)
    except Exception:
        pass

    raw = bytes(chunks)
    if len(sys.argv) > 1:
        with open(sys.argv[1], "wb") as f:
            f.write(raw)

    # Parse the harness's stderr instrument stream: PHASE stream/seal markers bracket
    # the streaming window; FH <n> lines are footerHeight changes. A change BETWEEN
    # those markers is a forced full footer repaint = a streaming flash.
    fh_changes_total = 0
    fh_changes_streaming = 0
    in_stream = False
    fh_values = []
    for ln in stderr_buf.decode("utf-8", "replace").splitlines():
        if ln == "PHASE stream":
            in_stream = True
        elif ln == "PHASE seal":
            in_stream = False
        elif ln.startswith("FH "):
            fh_changes_total += 1
            try:
                fh_values.append(int(ln[3:]))
            except ValueError:
                pass
            if in_stream:
                fh_changes_streaming += 1

    def total(matches):
        return sum(int(m or b"1") for m in matches)

    csi_hist: dict = {}
    for fb in RE_CSI.findall(raw):
        k = fb.decode("latin1")
        csi_hist[k] = csi_hist.get(k, 0) + 1

    screen_text = None
    garbage = None
    try:
        import pyte

        screen = pyte.Screen(COLS, ROWS)
        stream = pyte.ByteStream(screen)
        stream.feed(raw)
        lines = [screen.display[y].rstrip() for y in range(ROWS)]
        screen_text = "\n".join(lines)
        garbage = not screen_text.strip()
    except Exception as exc:
        screen_text = f"<pyte unavailable: {exc}>"

    return {
        "bytes": len(raw),
        "sealed": sealed_at is not None,
        "footer_resizes_streaming": fh_changes_streaming,
        "footer_resizes_total": fh_changes_total,
        "footer_heights": fh_values,
        "scroll_up": total(RE_SCROLL_UP.findall(raw)),
        "scroll_down": total(RE_SCROLL_DOWN.findall(raw)),
        "clear_screen": len(RE_CLEAR_SCREEN.findall(raw)),
        "csi_hist": dict(sorted(csi_hist.items(), key=lambda kv: -kv[1])),
        "screen": screen_text,
        "garbage": garbage,
    }


if __name__ == "__main__":
    print(json.dumps(run(), indent=2))
