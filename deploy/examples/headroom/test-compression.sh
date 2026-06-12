#!/bin/bash
# test-compression.sh — Test headroom compression directly (no LLM cost)
#
# Calls the headroom sidecar's /v1/compress endpoint inside the pod.
# No requests go to Anthropic/OpenAI — this only tests compression.
#
# Usage:
#   ./test-compression.sh                    # run all scenarios
#   ./test-compression.sh search             # run one scenario
#   NAMESPACE=my-ns POD_LABEL=app=my-app ./test-compression.sh  # custom pod

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-ingress}"
POD_LABEL="${POD_LABEL:-app=payload-processing}"
CONTAINER="${CONTAINER:-headroom}"
SCENARIO="${1:-all}"

POD=$(oc get pods -n "$NAMESPACE" -l "$POD_LABEL" -o jsonpath='{.items[0].metadata.name}')
if [ -z "$POD" ]; then
    echo "ERROR: No pod found with label $POD_LABEL in namespace $NAMESPACE"
    exit 1
fi

echo "Pod: $POD ($NAMESPACE)"
echo ""

run_test() {
    local name="$1"
    local python_code="$2"

    echo "=== $name ==="
    oc exec "$POD" -n "$NAMESPACE" -c "$CONTAINER" -- python3 -c "$python_code" 2>&1
    echo ""
}

# --- Scenario: Search results (RAG) ---
SEARCH_TEST='
import json, urllib.request
results = []
for i in range(50):
    results.append({
        "id": i, "title": "Document %d" % i,
        "url": "https://docs.example.com/article/%d" % i,
        "snippet": "This article covers topic %d comprehensively. Key areas include architecture design patterns, implementation best practices, performance optimization strategies, security hardening procedures, monitoring and alerting configuration, disaster recovery planning, capacity management, and operational runbook development. The content is based on production experience from organizations running at scale." % i,
        "score": round(0.99 - i * 0.01, 2),
        "metadata": {"author": "Author %d" % i, "updated": "2026-06-%02d" % (i % 28 + 1)}
    })
msgs = [
    {"role": "user", "content": "Find docs about Kubernetes security best practices"},
    {"role": "assistant", "content": None, "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "search_docs", "arguments": "{\"query\": \"kubernetes security\"}"}}]},
    {"role": "tool", "tool_call_id": "c1", "content": json.dumps(results)},
    {"role": "user", "content": "Which are the top 5 most relevant?"}
]
p = json.dumps({"model": "claude-opus-4-6", "messages": msgs}).encode()
r = urllib.request.urlopen(urllib.request.Request("http://localhost:8787/v1/compress", data=p, headers={"Content-Type": "application/json"}), timeout=60)
d = json.loads(r.read())
tb, ta, ts = d.get("tokens_before", 0), d.get("tokens_after", 0), d.get("tokens_saved", 0)
pct = (ts * 100.0 / tb) if tb > 0 else 0
print("  Before: %d tokens" % tb)
print("  After:  %d tokens" % ta)
print("  Saved:  %d tokens (%.1f%%)" % (ts, pct))
transforms = [t for t in d.get("transforms_applied", []) if "protected" not in t]
if transforms:
    print("  Transforms: %s" % ", ".join(transforms))
else:
    print("  Transforms: (none — all messages protected)")
'

# --- Scenario: API response (JSON) ---
API_TEST='
import json, urllib.request
pods = []
for i in range(30):
    pods.append({
        "metadata": {"name": "pod-%d" % i, "namespace": "production", "labels": {"app": "service-%d" % (i %5), "version": "v1.%d" % i}},
        "status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}], "containerStatuses": [{"name": "main", "ready": True, "restartCount": i %3, "image": "registry.example.com/service-%d:v1.%d.0" % (i %5, i)}]},
        "spec": {"nodeName": "node-%d" % (i %4), "containers": [{"name": "main", "resources": {"requests": {"cpu": "100m", "memory": "256Mi"}, "limits": {"cpu": "500m", "memory": "512Mi"}}}]}
    })
msgs = [
    {"role": "user", "content": "Show me all pods in the production namespace"},
    {"role": "assistant", "content": None, "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "kubectl_get", "arguments": "{\"resource\": \"pods\", \"namespace\": \"production\"}"}}]},
    {"role": "tool", "tool_call_id": "c1", "content": json.dumps({"items": pods})},
    {"role": "user", "content": "Which pods have been restarting?"}
]
p = json.dumps({"model": "claude-opus-4-6", "messages": msgs}).encode()
r = urllib.request.urlopen(urllib.request.Request("http://localhost:8787/v1/compress", data=p, headers={"Content-Type": "application/json"}), timeout=60)
d = json.loads(r.read())
tb, ta, ts = d.get("tokens_before", 0), d.get("tokens_after", 0), d.get("tokens_saved", 0)
pct = (ts * 100.0 / tb) if tb > 0 else 0
print("  Before: %d tokens" % tb)
print("  After:  %d tokens" % ta)
print("  Saved:  %d tokens (%.1f%%)" % (ts, pct))
transforms = [t for t in d.get("transforms_applied", []) if "protected" not in t]
if transforms:
    print("  Transforms: %s" % ", ".join(transforms))
'

# --- Scenario: Log dump ---
LOGS_TEST='
import json, urllib.request
lines = []
for i in range(200):
    level = ["INFO", "WARN", "ERROR", "DEBUG"][i %4]
    lines.append("2026-06-12T10:%02d:%02dZ %s [svc-%d] req=%d path=/api/resource/%d status=%d latency=%dms user=u-%d message=Processing request with timeout=30s retry=3 batch=100" % (i % 60, i % 60, level, i % 5, 10000 + i, i % 100, [200, 201, 400, 500][i % 4], 50 + i * 3 % 500, i % 20))
msgs = [
    {"role": "user", "content": "Show me the last 200 log lines from the production service"},
    {"role": "assistant", "content": None, "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "get_logs", "arguments": "{\"service\": \"production\", \"lines\": 200}"}}]},
    {"role": "tool", "tool_call_id": "c1", "content": "\n".join(lines)},
    {"role": "user", "content": "Find all ERROR entries and summarize the issues"}
]
p = json.dumps({"model": "claude-opus-4-6", "messages": msgs}).encode()
r = urllib.request.urlopen(urllib.request.Request("http://localhost:8787/v1/compress", data=p, headers={"Content-Type": "application/json"}), timeout=60)
d = json.loads(r.read())
tb, ta, ts = d.get("tokens_before", 0), d.get("tokens_after", 0), d.get("tokens_saved", 0)
pct = (ts * 100.0 / tb) if tb > 0 else 0
print("  Before: %d tokens" % tb)
print("  After:  %d tokens" % ta)
print("  Saved:  %d tokens (%.1f%%)" % (ts, pct))
transforms = [t for t in d.get("transforms_applied", []) if "protected" not in t]
if transforms:
    print("  Transforms: %s" % ", ".join(transforms))
'

# --- Scenario: File content (code) ---
CODE_TEST='
import json, urllib.request
code = "package main\n\nimport (\n\t\"context\"\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"net/http\"\n\t\"time\"\n)\n\n"
for i in range(30):
    code += "// Handler%d processes requests for resource type %d\n" % (i, i)
    code += "func Handler%d(w http.ResponseWriter, r *http.Request) {\n" % i
    code += "\tctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)\n"
    code += "\tdefer cancel()\n\n"
    code += "\tvar req struct {\n\t\tID   int    `json:\"id\"`\n\t\tName string `json:\"name\"`\n\t}\n"
    code += "\tif err := json.NewDecoder(r.Body).Decode(&req); err != nil {\n"
    code += "\t\thttp.Error(w, fmt.Sprintf(\"invalid request: %%v\", err), http.StatusBadRequest)\n"
    code += "\t\treturn\n\t}\n\n"
    code += "\tresult, err := processResource%d(ctx, req.ID, req.Name)\n" % i
    code += "\tif err != nil {\n\t\thttp.Error(w, err.Error(), http.StatusInternalServerError)\n\t\treturn\n\t}\n\n"
    code += "\tw.Header().Set(\"Content-Type\", \"application/json\")\n"
    code += "\tjson.NewEncoder(w).Encode(result)\n}\n\n"
msgs = [
    {"role": "user", "content": "Read the main.go file and review it"},
    {"role": "assistant", "content": None, "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\": \"main.go\"}"}}]},
    {"role": "tool", "tool_call_id": "c1", "content": code},
    {"role": "user", "content": "Any bugs or improvements?"}
]
p = json.dumps({"model": "claude-opus-4-6", "messages": msgs}).encode()
r = urllib.request.urlopen(urllib.request.Request("http://localhost:8787/v1/compress", data=p, headers={"Content-Type": "application/json"}), timeout=60)
d = json.loads(r.read())
tb, ta, ts = d.get("tokens_before", 0), d.get("tokens_after", 0), d.get("tokens_saved", 0)
pct = (ts * 100.0 / tb) if tb > 0 else 0
print("  Before: %d tokens" % tb)
print("  After:  %d tokens" % ta)
print("  Saved:  %d tokens (%.1f%%)" % (ts, pct))
transforms = [t for t in d.get("transforms_applied", []) if "protected" not in t]
if transforms:
    print("  Transforms: %s" % ", ".join(transforms))
'

# --- Scenario: Plain conversation (no tools — should show 0% savings) ---
CHAT_TEST='
import json, urllib.request
msgs = [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "What is Kubernetes?"},
    {"role": "assistant", "content": "Kubernetes is an open-source container orchestration platform."},
    {"role": "user", "content": "How does it compare to Docker Swarm?"}
]
p = json.dumps({"model": "claude-opus-4-6", "messages": msgs}).encode()
r = urllib.request.urlopen(urllib.request.Request("http://localhost:8787/v1/compress", data=p, headers={"Content-Type": "application/json"}), timeout=60)
d = json.loads(r.read())
tb, ta, ts = d.get("tokens_before", 0), d.get("tokens_after", 0), d.get("tokens_saved", 0)
pct = (ts * 100.0 / tb) if tb > 0 else 0
print("  Before: %d tokens" % tb)
print("  After:  %d tokens" % ta)
print("  Saved:  %d tokens (%.1f%%)" % (ts, pct))
print("  (Expected: 0%% — headroom protects conversation messages by design)")
'

# --- Run scenarios ---
case "$SCENARIO" in
    search) run_test "Search/RAG Results (50 documents)" "$SEARCH_TEST" ;;
    api)    run_test "API Response (30 K8s pods)" "$API_TEST" ;;
    logs)   run_test "Log Dump (200 lines)" "$LOGS_TEST" ;;
    code)   run_test "Code File (30 handlers)" "$CODE_TEST" ;;
    chat)   run_test "Plain Conversation (no tools)" "$CHAT_TEST" ;;
    all)
        echo "============================================"
        echo "  Headroom Compression Test Suite"
        echo "  (direct /v1/compress — no LLM cost)"
        echo "============================================"
        echo ""
        run_test "1. Search/RAG Results (50 documents)" "$SEARCH_TEST"
        run_test "2. API Response (30 K8s pods)" "$API_TEST"
        run_test "3. Log Dump (200 lines)" "$LOGS_TEST"
        run_test "4. Code File (30 handlers)" "$CODE_TEST"
        run_test "5. Plain Conversation (no tools — baseline)" "$CHAT_TEST"
        echo "============================================"
        echo "  Done. Scenarios 1-4 should show savings."
        echo "  Scenario 5 should show 0% (by design)."
        echo "============================================"
        ;;
    *) echo "Unknown scenario: $SCENARIO"; echo "Options: search, api, logs, code, chat, all"; exit 1 ;;
esac
