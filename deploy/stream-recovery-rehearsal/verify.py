#!/usr/bin/env python3
"""Host Python stdlib verifier. Failures are data and a nonzero process exit."""
import argparse
from collections import Counter
from decimal import Decimal
import http.client
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def write_json(path, value):
    with open(path, "w", encoding="utf-8") as f:
        json.dump(value, f, ensure_ascii=False, indent=2)
        f.write("\n")
    os.chmod(path, 0o600)


class Rig:
    def __init__(self, root):
        self.root = Path(root)
        self.m = json.load(open(self.root / "manifest.json", encoding="utf-8"))
        self.tag = self.m["tag"]
        self.port = self.m["port"]
        self.pg = self.tag + "-pg"
        self.rd = self.tag + "-redis"
        self.app = self.tag + "-app"
        self.ids = {}
        self.app_ip = None

    def docker(self, *args):
        p = subprocess.run(["docker", *args], capture_output=True, text=True, timeout=30)
        require(p.returncode == 0, "docker observation failed (inspect private logs)")
        return p.stdout

    def q(self, query):
        # Statement always via stdin; never put keys or SQL on the process command line.
        p = subprocess.run(["docker", "exec", "-i", self.pg, "psql", "-X", "-U", "sub2api", "-d", "sub2api", "-Atq", "-v", "ON_ERROR_STOP=1"], input=query + "\n", capture_output=True, text=True, timeout=30)
        require(p.returncode == 0, "isolated SQL observation failed")
        return json.loads(p.stdout)

    def preflight(self):
        for kind, name in [("network", self.tag + "-net"), ("container", self.app), ("container", self.pg), ("container", self.rd), ("container", self.tag + "-stub")]:
            obj = json.loads(self.docker(kind, "inspect", name))[0]
            labels = obj["Config"].get("Labels") if kind == "container" else obj.get("Labels")
            require((labels or {}).get("sr9.owner") == self.m["owner"], "resource ownership mismatch")
            if kind == "network":
                require(obj["Internal"] is True, "network must be internal")
            else:
                require(set(obj["NetworkSettings"]["Networks"]) == {self.tag + "-net"}, "container has an unexpected network")
                ports = obj.get("HostConfig", {}).get("PortBindings") or {}
                if name == self.app:
                    require(ports == {"8080/tcp": [{"HostIp": "127.0.0.1", "HostPort": str(self.port)}]}, "app must request only loopback publication")
                    self.app_ip = obj["NetworkSettings"]["Networks"][self.tag + "-net"]["IPAddress"]
                else:
                    require(not ports, "dependency port unexpectedly published")
        candidate = json.loads(self.docker("image", "inspect", self.m["image"]))[0]["Id"]
        running = json.loads(self.docker("container", "inspect", self.app))[0]["Image"]
        require(candidate == running, "app image differs from fixed candidate")
        if (self.root / "external-containers.json").exists():
            baseline = json.load(open(self.root / "external-containers.json"))
            names = self.docker("container", "ls", "-a", "--format", "{{.Names}}").splitlines()
            current = {}
            for name in names:
                if not name.startswith(self.tag + "-"):
                    obj = json.loads(self.docker("container", "inspect", name))[0]
                    current[name] = {"id": obj["Id"], "image": obj["Image"], "running": obj["State"]["Running"], "restart_policy": obj["HostConfig"]["RestartPolicy"]}
            require(current == baseline, "another container changed during rehearsal; cold replica state differs")
        # Only rows created in the isolated dump are queried. No live DB URL exists here.
        rows = self.q("SELECT coalesce(json_agg(row_to_json(c)), '[]'::json) FROM sr9.cases c;")
        self.ids = {r["name"]: r for r in rows}
        require(set(self.ids) == {c["name"] for c in self.m["cases"]}, "fixture row count differs")
        gate = self.q("SELECT json_build_object('oauth_enabled',(SELECT count(*) FROM accounts WHERE schedulable AND type != 'apikey'),'scheduled_tests',(SELECT count(*) FROM scheduled_test_plans WHERE enabled),'backup_enabled',(SELECT count(*) FROM settings WHERE key='backup_schedule' AND value::jsonb->>'enabled' IS DISTINCT FROM 'false'));")
        require(gate == {"oauth_enabled": 0, "scheduled_tests": 0, "backup_enabled": 0}, "isolated background jobs gate failed")
        deadline = time.monotonic() + 120
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        while time.monotonic() < deadline:
            try:
                with opener.open(f"http://{self.app_ip}:8080/health", timeout=2) as res:
                    doc = json.load(res)
                    if res.status == 200 and doc.get("status") == "ok":
                        break
            except (OSError, ValueError, urllib.error.URLError):
                pass
            time.sleep(1)
        else:
            raise AssertionError("app health did not become ready")
        return {"candidate_id": candidate, "network_internal": True, "loopback_port": self.port, "jobs": gate}

    def snapshot(self, case):
        r = self.ids[case["name"]]
        return self.q(f"""SELECT json_build_object(
'balance',(SELECT balance FROM users WHERE id={r['user_id']}),
'quota_used',(SELECT quota_used FROM api_keys WHERE id={r['key_id']}),
'rows',(SELECT coalesce(json_agg(json_build_object('id',id,'account_id',account_id,'request_id',request_id,'input_tokens',input_tokens,'output_tokens',output_tokens,'cache_read_tokens',cache_read_tokens,'cache_creation_tokens',cache_creation_tokens,'input_cost',input_cost,'output_cost',output_cost,'cache_read_cost',cache_read_cost,'cache_creation_cost',cache_creation_cost,'total_cost',total_cost,'actual_cost',actual_cost,'stream',stream) ORDER BY id),'[]'::json) FROM usage_logs WHERE api_key_id={r['key_id']}),
'accounts',(SELECT json_agg(json_build_object('id',id,'paused',temp_unschedulable_until>now(),'pause_seconds',greatest(0,extract(epoch from temp_unschedulable_until-now())),'pause_reason',temp_unschedulable_reason)) FROM accounts WHERE id IN ({r['account_a']},{r['account_b']}))) ;""")

    def stub_records(self, name):
        p = self.root / "stub-out" / "requests.jsonl"
        if not p.exists():
            return []
        return [r for line in p.read_text(encoding="utf-8").splitlines() if line and (r := json.loads(line)).get("case") == name]

    def request(self, case, turn=1, previous=None, invalid_key=False):
        proto = case["protocol"]
        prompt = f"SR9:{case['name']}:turn{turn} Write one, two, three."
        body = {"model": "gpt-5.1", "stream": True}
        path = {"responses": "/v1/responses", "chat": "/v1/chat/completions", "messages": "/v1/messages"}[proto]
        if proto == "responses":
            body["input"] = prompt
            if previous:
                body["previous_response_id"] = previous
        else:
            body["messages"] = [{"role": "user", "content": prompt}]
        if proto == "chat":
            body["stream_options"] = {"include_usage": True}
        if proto == "messages":
            body["max_tokens"] = 1000
        if case["kind"] in ("tool_half", "tool_handoff"):
            body["tools"] = [{"type": "function", "name": "lookup", "parameters": {"type": "object", "properties": {"x": {"type": "integer"}}}}]
        if case["kind"] == "custom":
            body["tools"] = [{"type": "namespace", "name": "terminal", "tools": [{"type": "custom", "name": "exec", "format": {"type": "text"}}]}]
        if case["kind"] == "parallel":
            body["tools"] = [{"type": "function", "function": {"name": "lookup", "parameters": {"type": "object", "properties": {"x": {"type": "integer"}}}}}]
        if case["kind"] == "reasoning":
            body["reasoning"] = {"effort": "medium", "summary": "auto"}
        if case["kind"] == "readonly":
            body["tools"] = [{"type": "web_search"}]
        if case["kind"].startswith("sideeffect"):
            body["tools"] = [{"type": "computer", "environment": "browser", "display_width": 100, "display_height": 100}]
        conn = http.client.HTTPConnection(self.app_ip, 8080, timeout=110)
        started = time.monotonic()
        headers = {"Authorization": "Bearer " + ("sk-invalid-synthetic" if invalid_key else case["key"]), "Content-Type": "application/json", "User-Agent": "sr9-fixture/" + case["name"], "X-Session-Id": self.tag + "-" + case["name"]}
        if proto == "messages":
            headers["x-api-key"] = headers["Authorization"].split(" ", 1)[1]
            headers["anthropic-version"] = "2023-06-01"
        try:
            conn.request("POST", path, json.dumps(body).encode(), headers)
            res = conn.getresponse()
            wire, arrivals = [], []
            while True:
                line = res.readline(65537)
                if not line:
                    break
                require(len(line) <= 65536, "SSE line exceeds fixture bound")
                wire.append(line)
                arrivals.append({"seconds": round(time.monotonic() - started, 3), "comment": line.startswith(b":"), "line": line.decode("utf-8").rstrip("\r\n")})
                require(sum(map(len, wire)) <= 2 * 1024 * 1024, "downstream output exceeds bound")
            result = {"http_status": res.status, "content_type": res.getheader("Content-Type", ""), "elapsed_seconds": round(time.monotonic() - started, 3), "wire": b"".join(wire).decode("utf-8"), "arrivals": arrivals}
        finally:
            conn.close()
        suffix = f"{case['name']}-turn{turn}" + ("-invalid-auth" if invalid_key else "")
        (self.root / (suffix + ".sse")).write_text(result["wire"], encoding="utf-8")
        write_json(self.root / (suffix + ".timing.json"), arrivals)
        write_json(self.root / (suffix + ".response.json"), result)
        return result


def frames(wire):
    values, done = [], 0
    for raw in wire.replace("\r\n", "\n").split("\n\n"):
        data = "\n".join(line[5:].lstrip() for line in raw.split("\n") if line.startswith("data:"))
        if not data:
            continue
        if data == "[DONE]":
            done += 1
        else:
            values.append(json.loads(data))
    return values, done


def analyse(result, case):
    require(result["http_status"] == 200, "expected HTTP 200 stream")
    require("text/event-stream" in result["content_type"], "expected SSE content type")
    values, done = frames(result["wire"])
    require(values, "stream contains no JSON events")
    kinds = Counter(v.get("type", "") for v in values)
    failures = [v for v in values if v.get("type") in ("error", "response.failed", "response.incomplete") or v.get("error")]
    negative = case["kind"] == "sideeffect_fail"
    if negative:
        require(failures, "side-effect interrupted stream must not claim success")
        require(kinds["response.completed"] == 0, "side-effect failure incorrectly completed")
    else:
        require(not failures, "intermediate failure leaked downstream")
    proto = case["protocol"]
    text, usage, logical = "", {}, None
    tools = []
    reasoning = []
    if proto == "responses":
        require(kinds["response.created"] == 1, "duplicate/missing logical response.created")
        sequences = [v.get("sequence_number") for v in values]
        require(sequences == list(range(len(sequences))), "Responses sequence_number not contiguous")
        completed = [v["response"] for v in values if v.get("type") == "response.completed"]
        require(len(completed) == (0 if negative else 1), "duplicate/missing Responses terminal")
        logical = next(v["response"]["id"] for v in values if v.get("type") == "response.created")
        require(all(v.get("response", {}).get("id", logical) == logical for v in values), "logical response identity changed")
        text = "".join(v.get("delta", "") for v in values if v.get("type") == "response.output_text.delta")
        if completed:
            output = completed[0].get("output", [])
            snapshot_text = "".join(p.get("text", "") for i in output if i.get("type") == "message" for p in i.get("content", []) if p.get("type") == "output_text")
            require(snapshot_text == text, "completed output differs from streamed text")
            usage = completed[0].get("usage", {})
            tools = [i for i in output if i.get("type") in ("function_call", "custom_tool_call")]
            reasoning = [i for i in output if i.get("type") == "reasoning"]
            require(completed[0].get("status") == "completed", "completed status missing")
        added = [v.get("item", {}).get("id") for v in values if v.get("type") == "response.output_item.added"]
        closed = [v.get("item", {}).get("id") for v in values if v.get("type") == "response.output_item.done"]
        if not negative:
            require(Counter(added) == Counter(closed), "output items are not paired")
        for tool in tools:
            if tool["type"] == "function_call":
                require(isinstance(json.loads(tool["arguments"]), dict), "committed tool JSON invalid")
    elif proto == "chat":
        require(done == 1, "Chat must emit [DONE] once")
        ids = {v["id"] for v in values if v.get("id")}
        require(len(ids) == 1, "Chat identity changed across attempts")
        terminal = []
        callmap = {}
        for v in values:
            if v.get("usage"):
                usage = v["usage"]
            for c in v.get("choices", []):
                d = c.get("delta", {})
                text += d.get("content", "")
                if d.get("reasoning_content"):
                    reasoning.append(d["reasoning_content"])
                if c.get("finish_reason"):
                    terminal.append(c["finish_reason"])
                for tool in d.get("tool_calls", []):
                    entry = callmap.setdefault(tool["index"], {"arguments": ""})
                    entry.update({k: v for k, v in tool.items() if k != "function"})
                    fn = tool.get("function", {})
                    entry["arguments"] += fn.get("arguments", "")
                    entry.update({k: v for k, v in fn.items() if k != "arguments"})
        require(len(terminal) == 1, "Chat terminal repeated or missing")
        tools = list(callmap.values())
        for t in tools:
            json.loads(t["arguments"])
    else:
        require(kinds["message_start"] == 1 and kinds["message_stop"] == 1, "Messages lifecycle repeated/missing")
        starts = [v["index"] for v in values if v.get("type") == "content_block_start"]
        stops = [v["index"] for v in values if v.get("type") == "content_block_stop"]
        require(Counter(starts) == Counter(stops), "Messages content block lifecycle unbalanced")
        for v in values:
            if v.get("type") == "content_block_delta" and v.get("delta", {}).get("type") == "text_delta":
                text += v["delta"].get("text", "")
            if v.get("type") == "message_start":
                usage.update(v["message"].get("usage", {}))
            if v.get("type") == "message_delta":
                usage.update(v.get("usage", {}))
        require(sum(v.get("type") == "message_delta" and bool(v.get("delta", {}).get("stop_reason")) for v in values) == 1, "Messages terminal reason repeated/missing")
    if case["kind"] in ("recover", "longwait", "reasoning"):
        require(text == "one\ntwo\nthree", "seam text is missing, repeated, or restarted")
    if case["kind"] in ("readonly", "sideeffect_pass"):
        require(text == "hosted-ok", "hosted success lost text")
        require(any(v.get("item", {}).get("type") == ("web_search_call" if case["kind"] == "readonly" else "computer_call") for v in values), "hosted tool output lost")
    if case["kind"] in ("tool_half", "tool_handoff"):
        require(len(tools) == 1 and tools[0].get("namespace") == "functions", "tool handoff lost complete call or namespace")
        require(json.loads(tools[0]["arguments"]) == {"x": 1}, "function argument content lost")
    if case["kind"] == "custom":
        require(len(tools) == 1 and tools[0].get("namespace") == "terminal" and tools[0].get("input") == "printf synthetic", "custom tool input/namespace lost")
    if case["kind"] == "parallel":
        require(len(tools) == 2 and {json.loads(t["arguments"])["x"] for t in tools} == {1, 2}, "parallel tool calls lost")
        require(all(t.get("namespace") == "functions" for t in tools), "parallel namespace lost")
        require(reasoning == ["private-thought"], "Chat reasoning channel lost")
    if case["kind"] == "reasoning":
        require(reasoning and "private-thought" not in text, "reasoning leaked into public text or was dropped")
        require(any(r.get("encrypted_content") for r in reasoning), "opaque reasoning metadata lost")
    if case["kind"] == "longwait":
        heartbeats = [a["seconds"] for a in result["arrivals"] if a["comment"] or '"type":"ping"' in a["line"]]
        require(result["elapsed_seconds"] >= 10, "longwait scenario did not wait")
        require(len(heartbeats) >= 3 and any(1 <= t <= 10 for t in heartbeats), "no repeated heartbeat during recovery waiting")
    return {"logical_id": logical, "text": text, "usage": usage, "events": dict(kinds), "tools": tools, "reasoning_items": len(reasoning), "elapsed_seconds": result["elapsed_seconds"]}


def compare_billing(before, after, records, case):
    old = {r["id"] for r in before["rows"]}
    rows = [r for r in after["rows"] if r["id"] not in old]
    require(len(rows) == len(records), "usage_logs must contain one row per billable stub attempt")
    require(len({r["request_id"] for r in rows}) == len(rows), "billing attempt request IDs duplicated")
    for r in rows:
        require(r["stream"] is True, "usage row did not retain stream flag")
        require(str(r["request_id"]).startswith("stream_recovery:"), "billing attempt missing stream_recovery ID")
        terms = sum(Decimal(str(r[k])) for k in ("input_cost", "output_cost", "cache_read_cost", "cache_creation_cost"))
        require(abs(terms - Decimal(str(r["total_cost"]))) <= Decimal("0.00000001"), "cost components do not sum")
        require(abs(Decimal(str(r["actual_cost"])) - Decimal(str(r["total_cost"]))) <= Decimal("0.00000001"), "unexpected rate multiplier or enhanced billing")
    expected = Counter()
    for r in records:
        u = r["usage"]
        expected["input_tokens"] += u["input_tokens"] - u["input_tokens_details"]["cached_tokens"]
        expected["output_tokens"] += u["output_tokens"]
        expected["cache_read_tokens"] += u["input_tokens_details"]["cached_tokens"]
    actual = {k: sum(r[k] for r in rows) for k in expected}
    require(actual == dict(expected), "usage tokens differ from stub attempts (including interrupted-hop usage)")
    charge = sum(Decimal(str(r["actual_cost"])) for r in rows)
    require(charge > 0, "billing did not charge known model gpt-5.1")
    delta = Decimal(str(before["balance"])) - Decimal(str(after["balance"]))
    quota = Decimal(str(after["quota_used"])) - Decimal(str(before["quota_used"]))
    require(abs(delta - charge) <= Decimal("0.00000005") and abs(quota - charge) <= Decimal("0.00000005"), "balance/quota deduction differs from usage_logs")
    return {"rows": rows, "token_totals": actual, "balance_delta": str(delta), "quota_delta": str(quota), "charge": str(charge)}


def run_case(rig, case):
    current = rig.snapshot(case)
    saved_before = rig.root / (case["name"] + ".before.json")
    recheck = bool(current["rows"])
    if recheck:
        require(saved_before.exists(), "original billing snapshot missing; choose fresh TAG")
        before = json.load(open(saved_before))
    else:
        before = current
        require(not rig.stub_records(case["name"]), "partial attempt evidence exists; choose fresh TAG rather than replay")
        write_json(saved_before, before)
    records_before = 0
    outputs = []
    previous = None
    turns = 3 if case["kind"] == "alias" else 1
    for turn in range(1, turns + 1):
        if recheck:
            result = json.load(open(rig.root / f"{case['name']}-turn{turn}.response.json"))
        else:
            result = rig.request(case, turn, previous)
        analysis = analyse(result, case)
        outputs.append(analysis)
        previous = analysis["logical_id"]
        if case["kind"] == "alias":
            require(analysis["text"] == f"answer-{turn}\ntail\ncomplete", "alias turn seam incorrect")
    records = rig.stub_records(case["name"])[records_before:]
    expected_hops = 2 if case["kind"] in ("recover", "longwait", "tool_half", "reasoning") else (6 if case["kind"] == "alias" else 1)
    require(len(records) == expected_hops, "unexpected number of upstream attempts; hosted side-effect replay may have occurred")
    expected_path = "/v1/chat/completions" if case["route"] == "rawchat" else "/v1/responses"
    require(all(r["path"] == expected_path for r in records), "upstream protocol route differs from fixture account")
    if expected_hops == 2:
        require([r["lane"] for r in records] == ["a", "b"], "recovery did not switch accounts A to B")
        history = json.dumps(records[1]["request"], ensure_ascii=False)
        require("previous_response_id" not in records[1]["request"], "unsafe upstream previous ID reused during account switch")
        if case["kind"] != "tool_half":
            require("one\\ntwo" in history, "recovery request omitted committed assistant text")
    if case["kind"] == "tool_handoff":
        require(not any(r["lane"] == "b" for r in records), "complete tool handoff was replayed")
    if case["kind"] == "alias":
        require([r["lane"] for r in records] == ["a", "b", "b", "a", "a", "b"], "multi-turn account/alias provenance incorrect")
        for turn in (2, 3):
            recs = [r for r in records if r["turn"] == turn]
            prior_upstream = [r for r in records if r["turn"] == turn - 1][-1]["response_id"]
            require(recs[0]["request"].get("previous_response_id") == prior_upstream, "logical alias did not resolve to latest upstream response")
            hist = json.dumps(recs[1]["request"])
            require("previous_response_id" not in recs[1]["request"], "cross-account alias kept old server context")
            for earlier in range(1, turn + 1):
                require(f"turn{earlier}" in hist, "cross-account fallback lost an earlier user turn")
                if earlier < turn:
                    require(f"answer-{earlier}" in hist, "cross-account fallback lost earlier assistant output")
    # Asynchronous billing has a bounded settling window, not a blind fixed sleep.
    deadline = time.monotonic() + 20
    after = rig.snapshot(case)
    while len(after["rows"]) < len(records) and time.monotonic() < deadline:
        time.sleep(0.25)
        after = rig.snapshot(case)
    billing = compare_billing(before, after, records, case)
    if any(r["failure"] for r in records) and case["kind"] != "alias":
        paused = [a for a in after["accounts"] if a.get("paused") and str(a.get("pause_reason", "")).startswith("[stream_recovery]")]
        require(paused and all(0 < a["pause_seconds"] <= 600 for a in paused), "account cooldown not written/read correctly")
    return {"outputs": outputs, "attempts": [{k: r[k] for k in ("lane", "path", "turn", "response_id", "failure", "retry_after", "usage")} for r in records], "billing": billing, "cooldown": after["accounts"]}


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--root", required=True)
    p.add_argument("--self-test", action="store_true")
    args = p.parse_args()
    if args.self_test:
        require(frames('data: {"x":1}\n\n: heartbeat\n\ndata: [DONE]\n\n') == ([{"x": 1}], 1), "SSE parser self test failed")
        try:
            require(False, "test")
        except AssertionError:
            pass
        else:
            raise AssertionError("assertion gate ineffective")
        print(json.dumps({"parser_self_test": "pass", "remote_validation": "not_run"}))
        return
    rig = Rig(args.root)
    report = {"tag": rig.tag, "status": "failed", "execution": "isolated_runtime", "cases": [], "passed": 0, "failed": 0}
    try:
        report["preflight"] = rig.preflight()
        sample = rig.m["cases"][0]
        before = len(rig.stub_records(sample["name"]))
        unauth = rig.request(sample, invalid_key=True)
        require(unauth["http_status"] == 401, "invalid key must fail real authentication")
        require(len(rig.stub_records(sample["name"])) == before, "invalid key reached upstream")
        report["auth"] = {"invalid_key_http_status": 401, "upstream_attempts": 0}
        for case in rig.m["cases"]:
            entry = {k: case[k] for k in ("name", "route", "protocol", "kind")}
            try:
                entry["observations"] = run_case(rig, case)
                entry["status"] = "passed"
                report["passed"] += 1
            except Exception as exc:
                entry["status"] = "failed"
                entry["error"] = str(exc)[:500]
                # Retain numeric DB state and stub hop summaries even for a failed assertion.
                try:
                    entry["observations"] = {
                        "database": rig.snapshot(case),
                        "attempts": [{k: r[k] for k in ("lane", "path", "turn", "response_id", "failure", "retry_after", "usage")} for r in rig.stub_records(case["name"])],
                        "sse_files": sorted(p.name for p in rig.root.glob(case["name"] + "-turn*.sse")),
                    }
                except Exception:
                    entry["observation_error"] = "post-failure evidence query unavailable"
                report["failed"] += 1
            report["cases"].append(entry)
            write_json(rig.root / "report.json", report)
            print(json.dumps({"case": case["name"], "status": entry["status"]}), flush=True)
        # Fresh Redis must hold the real provenance state. Only aggregate info leaves.
        scan = rig.docker("exec", rig.rd, "/bin/sh", "-c", 'REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli --scan --pattern "*stream-recovery*"')
        keys = scan.splitlines()
        report["redis"] = {"stream_recovery_key_count": len(keys)}
        require(keys, "no shared Redis recovery context observed")
        report["status"] = "passed" if report["failed"] == 0 else "failed"
    except Exception as exc:
        report["fatal_error"] = str(exc)[:500]
        report["failed"] += 1
    finally:
        write_json(rig.root / "report.json", report)
    print(json.dumps({"tag": rig.tag, "status": report["status"], "passed": report["passed"], "failed": report["failed"], "report": str(rig.root / "report.json")}))
    raise SystemExit(0 if report["status"] == "passed" else 1)


if __name__ == "__main__":
    main()
