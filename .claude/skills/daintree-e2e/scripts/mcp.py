#!/usr/bin/env python3
"""Minimal Daintree MCP client for the daintree-e2e skill (stdlib only).

Subcommands:
  mcp.py creds                     # print "URL<TAB>TOKEN" resolved from env or newest log
  mcp.py call <tool> [json-args]   # call one MCP tool, print its JSON result
  mcp.py <tool> [json-args]        # shorthand for `call` (e.g. mcp.py terminal.list '{}')
  mcp.py close-all                 # close EVERY open terminal (reset the project between runs)

Creds come from $DAINTREE_MCP_URL / $DAINTREE_MCP_TOKEN — ENV ONLY.

There is deliberately no fallback that reads them out of a log file. The runtime
removed the `mcp.credentials` log line on purpose: the token authorises system-tier
Daintree actions for its whole validity window, and a log file outlives it. A script
that goes looking for it there is broken twice over — it finds nothing on any current
build, and it teaches the next reader that credentials live in logs, which is exactly
the pressure that would get the unsafe line put back.

Take the values from the RUNNING assistant's own environment while it still holds
them, e.g.:

    eval "$(ps eww -o command= -p <pid> | tr ' ' '\n' | grep -E '^DAINTREE_MCP_(URL|TOKEN)=' | sed 's/^/export /')"

Daintree tokens expire ~12 minutes: on HTTP 401 the token is stale — open a Daintree
session and re-export from that process.
"""
import sys, os, json, urllib.request, urllib.error


def resolve_creds():
    url = os.environ.get("DAINTREE_MCP_URL")
    tok = os.environ.get("DAINTREE_MCP_TOKEN")
    if url and tok:
        return url, tok
    sys.exit("no MCP creds: export DAINTREE_MCP_URL and DAINTREE_MCP_TOKEN from a running "
             "Daintree-launched assistant process. They are NOT in the debug log and must "
             "not be put back there — the token stays powerful for its whole lifetime.")


def _post(url, tok, body, sid=None):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    req.add_header("Authorization", "Bearer " + tok)
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json, text/event-stream")
    if sid:
        req.add_header("Mcp-Session-Id", sid)
    resp = urllib.request.urlopen(req, timeout=90)
    return resp, resp.read().decode(errors="ignore")


def _parse(text):
    # Streamable-HTTP replies come back as SSE (event:/data:) OR plain JSON.
    if text.lstrip().startswith(("event:", "data:")):
        out = None
        for line in text.splitlines():
            line = line.strip()
            if line.startswith("data:"):
                try:
                    out = json.loads(line[5:].strip())
                except Exception:
                    pass
        return out
    try:
        return json.loads(text)
    except Exception:
        return {"_raw": text[:500]}


def _session(url, tok):
    resp, _ = _post(url, tok, {"jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": {"protocolVersion": "2025-06-18", "capabilities": {},
                   "clientInfo": {"name": "daintree-e2e", "version": "0"}}})
    sid = resp.headers.get("Mcp-Session-Id")
    _post(url, tok, {"jsonrpc": "2.0", "method": "notifications/initialized"}, sid)
    return sid


def _call(url, tok, sid, name, args):
    _, text = _post(url, tok, {"jsonrpc": "2.0", "id": 2, "method": "tools/call",
                               "params": {"name": name, "arguments": args}}, sid)
    return _parse(text)


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    cmd = sys.argv[1]
    url, tok = resolve_creds()
    if cmd == "creds":
        print(url + "\t" + tok)
        return
    sid = _session(url, tok)
    if cmd == "close-all":
        d = _call(url, tok, sid, "terminal.list", {}) or {}
        terms = d.get("result", {}).get("structuredContent", {}).get("terminals", [])
        ids = [t["id"] for t in terms]
        if not ids:
            print("no terminals to close (project clean)")
            return
        print("closing %d terminal(s): %s" % (len(ids), ", ".join(ids)))
        print(json.dumps(_call(url, tok, sid, "terminal.close", {"terminalIds": ids})))
        return
    name = sys.argv[2] if cmd == "call" else cmd
    if cmd == "call":
        raw = sys.argv[3] if len(sys.argv) > 3 else "{}"
    else:
        raw = sys.argv[2] if len(sys.argv) > 2 else "{}"
    print(json.dumps(_call(url, tok, sid, name, json.loads(raw))))


if __name__ == "__main__":
    try:
        main()
    except urllib.error.HTTPError as e:
        sys.exit("MCP HTTP %d: %s — the token likely expired (~12 min TTL); open a "
                 "Daintree session for a fresh one." % (e.code, e.reason))
    except urllib.error.URLError as e:
        sys.exit("MCP unreachable: %s — is Daintree running?" % e.reason)
