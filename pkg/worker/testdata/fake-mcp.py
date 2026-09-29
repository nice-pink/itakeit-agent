# A minimal stdio MCP server for TestClaudeCodeMCPLive: three tools, and a log
# of every call with the FAKE_MCP_TOKEN it got from mcp.json.
import json, os, sys
log = os.environ.get("MCP_LOG", "/dev/null")
tools = [{"name": n, "description": d, "inputSchema": {"type": "object", "properties": {"q": {"type": "string"}}}}
         for n, d in [("lookup", "Look up a fact"), ("change", "Change a setting"), ("unlisted", "Another tool")]]
for line in sys.stdin:
    m = json.loads(line)
    if "id" not in m:
        continue
    meth, res = m.get("method"), {}
    if meth == "initialize":
        res = {"protocolVersion": m["params"].get("protocolVersion", "2024-11-05"), "capabilities": {"tools": {}}, "serverInfo": {"name": "fake", "version": "1"}}
    elif meth == "tools/list":
        res = {"tools": tools}
    elif meth == "tools/call":
        name = m["params"]["name"]
        with open(log, "a") as f:
            f.write(f"called {name} secret={os.environ.get('FAKE_MCP_TOKEN', 'unset')}\n")
        res = {"content": [{"type": "text", "text": f"{name} ok"}]}
    print(json.dumps({"jsonrpc": "2.0", "id": m["id"], "result": res}), flush=True)
