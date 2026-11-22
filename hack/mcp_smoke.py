#!/usr/bin/env python3
"""Raw stdio smoke test for `velruna mcp` — independent of the Go SDK.

Acts like a spec-compliant client: sends one JSON-RPC message, awaits its
response, keeping stdin open in between (newline-delimited JSON, MCP stdio
framing). Runs the server from /tmp on purpose: VS Code may spawn it from
an arbitrary cwd, so this also proves config lookup works from anywhere.
Expects the fact KEY to exist in the store.

Usage: python3 hack/mcp_smoke.py
"""

import json
import os
import select
import subprocess
import sys
import time
from pathlib import Path

BIN = str(Path(__file__).resolve().parent.parent / "velruna")
KEY = "demo:api:port"


def main() -> int:
    proc = subprocess.Popen(
        [BIN, "mcp"],
        cwd="/tmp",
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        bufsize=0,
    )

    buf = b""

    def send(obj) -> None:
        proc.stdin.write(json.dumps(obj).encode() + b"\n")
        proc.stdin.flush()

    def await_msg(timeout: float):
        nonlocal buf
        deadline = time.time() + timeout
        while b"\n" not in buf:
            remaining = deadline - time.time()
            if remaining <= 0:
                return None
            readable, _, _ = select.select([proc.stdout], [], [], remaining)
            if not readable:
                return None
            chunk = os.read(proc.stdout.fileno(), 1 << 16)
            if not chunk:
                return None
            buf += chunk
        line, buf = buf.split(b"\n", 1)
        return json.loads(line) if line.strip() else None

    def brief(obj, limit: int = 400) -> str:
        if obj is None:
            return "NO RESPONSE"
        return json.dumps(obj, ensure_ascii=False)[:limit]

    send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
        "protocolVersion": "2025-06-18",
        "capabilities": {},
        "clientInfo": {"name": "py-smoke", "version": "0"},
    }})
    failures = []
    init = await_msg(10)
    print("initialize  :", brief(init, 160))
    if not (init and "result" in init):
        failures.append("initialize")

    send({"jsonrpc": "2.0", "method": "notifications/initialized"})
    send({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
    tools = await_msg(10)
    if tools and "result" in tools:
        names = [t.get("name") for t in tools["result"].get("tools", [])]
        print("tools/list  :", len(names), "tools:", ", ".join(names))
        if "velruna_get" not in names:
            failures.append("tools/list missing velruna_get")
    else:
        print("tools/list  :", brief(tools))
        failures.append("tools/list")

    send({"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {
        "name": "velruna_get", "arguments": {"key": KEY},
    }})
    call = await_msg(20)
    print("tools/call  :", brief(call, 800))
    ok = False
    if call and "result" in call:
        result = call["result"]
        # `found` is a TOP-LEVEL field of velruna_get's output; the fact view
        # has no such field. A handler error arrives as a normal result with
        # isError=true, and a missing fact is found=false: neither may pass.
        ok = not result.get("isError") and (result.get("structuredContent") or {}).get("found") is True
    if not ok:
        failures.append("tools/call")

    # 0.9 headline: a successor link round-trips through patch, get and the
    # journal. The workflow creates demo:api:new next to the key under test.
    send({"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {
        "name": "velruna_patch",
        "arguments": {"key": KEY, "superseded_by": "demo:api:new"},
    }})
    patched = await_msg(20)
    print("patch link  :", brief(patched, 400))
    if not (patched and "result" in patched and not patched["result"].get("isError")):
        failures.append("patch superseded_by")
    else:
        send({"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": {
            "name": "velruna_get", "arguments": {"key": KEY},
        }})
        again = await_msg(20)
        fact = {}
        if again and "result" in again:
            fact = (again["result"].get("structuredContent") or {}).get("fact") or {}
        print("link read   :", fact.get("superseded_by"))
        if fact.get("superseded_by") != "demo:api:new":
            failures.append("superseded_by roundtrip")

    proc.stdin.close()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
    print("server exit :", proc.returncode)
    err = proc.stderr.read().decode(errors="replace").strip()
    if err:
        print("stderr      :", err[:500])
    if failures:
        # A smoke that cannot fail is decoration: a silent server would have
        # printed NO RESPONSE and exited 0. The workflow step must go red.
        print("FAILED:", "; ".join(failures), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
