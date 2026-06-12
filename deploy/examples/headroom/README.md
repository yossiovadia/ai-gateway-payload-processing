# Headroom Context Compression Plugin

Compresses old tool outputs (file reads, logs, API responses) in the BBR pipeline
before requests reach the LLM provider. Reduces token costs with zero quality loss.

Upstream: https://github.com/chopratejas/headroom (28k+ stars)

## Architecture (v2 — Selective Tool Output Compression)

The Go plugin handles selection logic (what to compress vs protect). The sidecar
is a raw compression service — it compresses whatever it receives, no protection logic.

```
                     REQUEST FLOW

Client                   BBR Plugin Chain                           Provider
  │                          │                                         │
  │  messages: [             │                                         │
  │    user: "read file"     │                                         │
  │    tool_result: 50K   ──►│  headroom plugin:                       │
  │    assistant: "found..." │  1. Walk messages                       │
  │    user: "fix the bug"   │  2. Find tool_result older than N turns │
  │    tool_result: 30K   ──►│  3. Send to sidecar /v1/compress-raw    │
  │    assistant: "fixing.." │  4. Replace with compressed content     │
  │    user: "run tests"     │                                         │
  │  ]                       │         ┌─────────────┐                 │
  │                          │         │  Sidecar     │                 │
  │  120K input tokens       │────────►│  :8788       │                 │
  │                          │         │  compress-raw│                 │
  │                          │◄────────│  (Kompress)  │                 │
  │                          │         └─────────────┘                 │
  │                          │                                         │
  │                          │  Compressed: 120K → 45K tokens ────────►│
```

### What Gets Compressed vs Protected

| Message Type | Compress? | Why |
|---|---|---|
| `tool` result (old, > N turns) | **YES** | File reads, logs, diffs — bulk of tokens |
| `tool` result (last 2 turns) | No | Model might reference recent tool output |
| `assistant` (text) | No | Model's reasoning, must preserve |
| `user` (text) | No | User intent, must preserve |
| `system` prompt | No | Instructions, must preserve |

## Proven Results

### E2E Through Gateway (real Claude responses)

| Test | Input Tokens | Compressed | Saved | Quality |
|------|-------------|-----------|-------|---------|
| Agent debugging conversation | 273 | 129 | **53%** | Correct answer, no degradation |
| Search/RAG (50 documents) | 3,781 | 1,706 | **55%** | Same top-5 docs, same scores |

### Direct Compression Tests (no LLM cost)

| Content Type | Tokens Before | After | Saved |
|---|---|---|---|
| Search/RAG (50 docs) | 6,216 | 2,391 | **61.5%** |
| K8s API (30 pods JSON) | 4,991 | 1,483 | **70.3%** |
| Log lines (50 lines, Kompress ML) | 700 | 246 | **65%** |
| Plain conversation (no tools) | 53 | 53 | 0% (by design) |

### Cost Impact

At Claude Opus pricing ($15/M input tokens):
- Per request with tool outputs: ~$0.03 saved
- Per 1M requests: ~$31,000 saved

## Two-Service Sidecar

The sidecar runs two servers:
- **Port 8787**: Standard headroom proxy (healthz, stats)
- **Port 8788**: Raw compression service (`/v1/compress-raw`) — calls
  `ContentRouter.compress()` directly with Kompress ML model pre-loaded

### Dockerfile

```dockerfile
FROM python:3.11-slim

RUN pip install --no-cache-dir "headroom-ai[proxy]==0.25.0" onnxruntime

# Writable dirs for OpenShift random UID
ENV HF_HOME=/opt/huggingface
ENV HOME=/opt/app
ENV HEADROOM_DATA_DIR=/opt/headroom-data
RUN mkdir -p /opt/huggingface /opt/app /opt/headroom-data

# Pre-download Kompress ONNX model + ModernBERT tokenizer (no runtime downloads)
RUN python -c "\
from huggingface_hub import snapshot_download; \
snapshot_download('chopratejas/kompress-v2-base'); \
snapshot_download('answerdotai/ModernBERT-base')"

RUN chmod -R 777 /opt/huggingface /opt/app /opt/headroom-data

COPY compress-raw-server.py /opt/app/compress-raw-server.py

EXPOSE 8787 8788
ENTRYPOINT ["python3", "/opt/app/compress-raw-server.py"]
```

### Key Deployment Lessons

1. **`onnxruntime` must be installed separately** — `headroom-ai[proxy]` does NOT
   include it. Without onnxruntime, Kompress silently falls back to no compression.

2. **Pre-download BOTH models at build time**:
   - `chopratejas/kompress-v2-base` — Kompress ONNX model (~274MB int8)
   - `answerdotai/ModernBERT-base` — tokenizer used by Kompress
   
   Runtime downloads fail due to network restrictions in the cluster.

3. **OpenShift runs containers as a random UID** (e.g., `1000710000`), NOT root.
   - `/root/.cache/` is NOT writable → set `HF_HOME=/opt/huggingface`
   - `HOME` must be writable → set `HOME=/opt/app`
   - All data directories need `chmod 777` during build

4. **CPU requirements** — Kompress ML inference needs real CPU.
   - 500m (half core): too slow, timeouts
   - 4 cores: ~3s per compression call — acceptable for LLM requests (2-30s)
   - JSON compression (smart_crusher): instant, no ML needed

5. **Memory** — at least 4Gi for the ONNX model + inference buffers.

6. **Readiness probe** — use port 8788 path `/readyz` (the compress-raw server).

## Plugin Configuration

```yaml
- type: headroom
  name: headroom
  json:
    headroomURL: "http://localhost:8787"
    rawURL: "http://localhost:8788"
    timeoutSeconds: 10
    failOpen: true
    protectRecentTurns: 2
    minCompressChars: 500
```

| Field | Default | Description |
|-------|---------|-------------|
| `headroomURL` | (required) | Headroom proxy URL |
| `rawURL` | same as headroomURL | Raw compression service URL (compress-raw-server) |
| `timeoutSeconds` | `10` | HTTP timeout for compression calls |
| `failOpen` | `true` | Pass through uncompressed if sidecar is down |
| `protectRecentTurns` | `2` | Number of recent turns to protect from compression |
| `minCompressChars` | `500` | Minimum tool output size (chars) to attempt compression |

## A/B Evaluation

Compare responses with and without compression using the `X-Headroom-Bypass: true` header:

```bash
# With compression
curl -X POST "$GATEWAY/v1/chat/completions" \
  -H "Authorization: Bearer $API_KEY" \
  -d '{"model":"claude-opus-4-6","messages":[...]}'

# Without compression (bypass)
curl -X POST "$GATEWAY/v1/chat/completions" \
  -H "Authorization: Bearer $API_KEY" \
  -H "X-Headroom-Bypass: true" \
  -d '{"model":"claude-opus-4-6","messages":[...]}'
```

See `eval-headroom.sh` for automated A/B evaluation.
See `test-compression.sh` for direct compression testing (no LLM cost).

## Why Only Tool Outputs?

Typical Claude Code session token breakdown:

```
System prompt:        ~5K    (3%)     ← small, don't touch
User messages:        ~3K    (2%)     ← small, don't touch
Assistant reasoning:  ~15K   (10%)    ← important, don't touch
Tool results:         ~120K  (80%)    ← FILE READS, LOGS, DIFFS
Other:                ~7K    (5%)     ← metadata
```

Tool results are 80% of tokens. Compressing them at 53-70% saves ~60-84K tokens
per request. Compressing user/assistant messages would save 2-10% of tokens and
risk quality degradation.

## Why /v1/compress-raw Instead of /v1/compress?

Headroom's standard `/v1/compress` endpoint is designed for **proxy mode** with
session tracking. It protects all "recent" messages from compression. In the gateway
flow (stateless, no session), every request is treated as "turn 1" — everything is
"recent" — so nothing gets compressed.

The v2 architecture solves this by moving selection logic into the Go plugin:
- **Go plugin** decides WHAT to compress (old tool results only)
- **Sidecar** `/v1/compress-raw` compresses WHATEVER it receives (no protection)

This gives us session-like behavior without actual session state.

## Known Issues

- **ext-proc response headers disabled**: The EnvoyFilter sets `response_body_mode: NONE`
  for external providers. Compression stats are in BBR logs and CycleState only,
  not response headers.

- **Kompress latency**: ~3s per compression call on 4 CPU cores (ONNX inference,
  CPU-only build). All testing was done on CPU. With `onnxruntime-gpu` on a GPU
  node, inference would be <100ms — the Kompress model is ~261MB, trivial for
  modern GPUs. JSON compression (smart_crusher) is instant on CPU, no ML needed.

- **First-call cold start**: The Kompress model loads lazily on first `/v1/compress-raw`
  call (~1-4s). Subsequent calls are at inference speed. Consider a startup probe
  or warmup request.

- **`[transformers] PyTorch was not found`**: Benign warning. The proxy uses
  onnxruntime, not PyTorch.

## Verifying Savings

1. **BBR pod logs**: `headroom compression applied toolResults=N tokensBefore=X tokensAfter=Y tokensSaved=Z`
2. **Metering dashboard**: Compare token counts with and without headroom
3. **Test scripts**: `test-compression.sh` (direct, no cost) and `eval-headroom.sh` (A/B via Claude)
