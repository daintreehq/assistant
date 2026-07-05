#!/usr/bin/env python3
"""Minimal Daintree MCP client for the daintree-e2e skill (stdlib only).

Subcommands:
  mcp.py creds                     # print "URL<TAB>TOKEN" resolved from env or newest log
  mcp.py call <tool> [json-args]   # call one MCP tool, print its JSON result
  mcp.py <tool> [json-args]        # shorthand for `call` (e.g. mcp.py terminal.list '{}')
  mcp.py close-all                 # close EVERY open terminal (reset the project between runs)

Creds come from $DAINTREE_MCP_URL / $DAINTREE_MCP_TOKEN, else the newest
~/.daintree/logs/*.log 'mcp.credentials' line. Daintree tokens expire ~12 minutes:
on HTTP 401 the token is stale — open a Daintree session to mint a fresh one.
"""
import sys, os, re, json, glob, urllib.request, urllib.error


def resolve_creds():
    url = os.environ.get("DAINTREE_MCP_URL")
    tok = os.environ.get("DAINTREE_MCP_TOKEN")
    if url and tok:
        return url, tok
    logs = sorted(glob.glob(os.path.expanduser("~/.daintree/logs/*.log")),
                  key=os.path.getmtime, reverse=True)
    for log in logs:
        u = t = None
        try:
            with open(log, errors="ignore") as f:
                for line in f:
                    if "mcp.credentials" in line and "url=http" in line:
                        mu = re.search(r"url=(\S+)", line)
                        mt = re.search(r"token=(\S+)", line)
                        if mu and mt:
                            u, t = mu.group(1), mt.group(1)  # keep the LAST creds in this log
        except OSError:
            continue
        if u and t:
            return u, t
    sys.exit("no MCP creds: set DAINTREE_MCP_URL/TOKEN, or open a Daintree session so a "
             "fresh mcp.credentials line lands in ~/.daintree/logs")


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
