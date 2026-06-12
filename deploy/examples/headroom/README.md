# Headroom Context Compression Plugin

Headroom compresses LLM context (tool outputs, RAG chunks, conversation history)
before requests reach the provider. Same answers, fewer tokens.

Upstream: https://github.com/chopratejas/headroom (28k+ stars)

## How It Works

The headroom plugin sits in the BBR pipeline after model-provider-resolver and before
api-translation. On each request:

1. Extracts the `messages` array from the request body
2. Sends them to the headroom sidecar (`POST /v1/compress`) for compression
3. Replaces the messages with the compressed version
4. Writes compression stats to CycleState (for metering integration)

## Sidecar Image

The headroom sidecar runs the headroom proxy server. Key requirements learned from
deploying on OpenShift:

### Dockerfile

```dockerfile
FROM python:3.11-slim

RUN pip install --no-cache-dir "headroom-ai[proxy]==0.25.0" onnxruntime

# Pre-download the Kompress ONNX model — baked into the image, no runtime downloads.
# headroom-ai[proxy] does NOT include onnxruntime — must be installed separately.
# The Kompress ML compressor needs the ONNX model files from HuggingFace.
ENV HF_HOME=/opt/huggingface
RUN mkdir -p /opt/huggingface /opt/app /opt/headroom-data && \
    chmod -R 777 /opt/huggingface /opt/app /opt/headroom-data && \
    python -c "from huggingface_hub import snapshot_download; snapshot_download('chopratejas/kompress-v2-base')" && \
    chmod -R 755 /opt/huggingface

# Writable home dir for OpenShift (runs as random UID, not root)
ENV HOME=/opt/app
ENV HEADROOM_DATA_DIR=/opt/headroom-data

EXPOSE 8787
ENTRYPOINT ["headroom", "proxy", "--port", "8787", "--host", "0.0.0.0"]
```

### Key Lessons

1. **`onnxruntime` must be installed separately** — `headroom-ai[proxy]` does NOT include it.
   Without onnxruntime, the Kompress ML compressor silently falls back to no compression.

2. **Pre-download the Kompress model at build time** — The model is at
   `chopratejas/kompress-v2-base` on HuggingFace (~600MB fp32, ~274MB int8).
   Runtime downloads may fail due to network restrictions in the cluster.

3. **OpenShift runs containers as a random UID** (e.g., `1000710000`), NOT root.
   - `/root/.cache/` is NOT writable → set `HF_HOME=/opt/huggingface`
   - `HOME` must be a writable directory → set `HOME=/opt/app`
   - All data directories need `chmod 777` during build

4. **Memory requirements** — The ONNX model loading needs at least 1Gi.
   Set sidecar resource limits to at least 4Gi for production use
   (the model is ~600MB + onnxruntime overhead + headroom buffers).

5. **Startup time** — First request takes 4-6 seconds (model loading). Subsequent
   requests take ~50-250ms for compression. Set readiness probe with
   `initialDelaySeconds: 15`.

6. **Readiness probe path** — headroom uses `/readyz` (not `/healthz`).

## Deployment on OpenShift

See the [sandbox deploy runbook](../../docs/sandbox-deploy-runbook.md) for the full
list of cluster patches needed after deploying a new image.

### Plugin Chain

```yaml
plugins:
  - type: body-field-to-header
    name: model-extractor
    json:
      fieldName: model
      headerName: X-Gateway-Model-Name
  - type: external-metering
    name: metering-check
    json:
      meteringURL: "http://metering-service.openshift-ingress.svc:8080"
      timeoutSeconds: 5
      featureKey: "inference-tokens"
      source: "maas-gateway"
      failOpen: true
  - type: headroom
    name: headroom
    json:
      headroomURL: "http://localhost:8787"
      timeoutSeconds: 10
      failOpen: true
  - type: model-provider-resolver
    name: model-provider-resolver
  - type: api-translation
    name: api-translation
  - type: apikey-injection
    name: apikey-injection
```

### Sidecar Container Spec

```yaml
- name: headroom
  image: image-registry.openshift-image-registry.svc:5000/openshift-ingress/headroom-sidecar:latest
  ports:
    - containerPort: 8787
      name: headroom
      protocol: TCP
  resources:
    requests:
      cpu: 100m
      memory: 1Gi
    limits:
      cpu: "1"
      memory: 4Gi
  readinessProbe:
    httpGet:
      path: /readyz
      port: 8787
    initialDelaySeconds: 15
    periodSeconds: 10
```

### Building the Sidecar Image on OpenShift

```bash
# Create BuildConfig (first time only)
oc new-build --binary --strategy=docker --name=headroom-sidecar -n openshift-ingress

# Build (creates Dockerfile in temp dir, builds on cluster)
TMPDIR=$(mktemp -d)
# ... write Dockerfile (see above) ...
oc start-build headroom-sidecar -n openshift-ingress --from-dir="$TMPDIR" --follow
```

## Configuration

Plugin config (in Helm values under `plugins`):

| Field | Default | Description |
|-------|---------|-------------|
| `headroomURL` | (required) | URL of the headroom service (e.g., `http://localhost:8787`) |
| `timeoutSeconds` | `10` | HTTP timeout for compress calls |
| `failOpen` | `true` | Pass through uncompressed if headroom is down |
| `compressConfig` | `null` | Forwarded to headroom's compress endpoint (target_ratio, protect_recent, etc.) |

## A/B Evaluation

To compare responses with and without compression, use the `X-Headroom-Bypass: true` header:

```bash
# With compression
curl -X POST "$GATEWAY/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d '{"model":"claude-opus-4-6","max_tokens":200,"messages":[...]}'

# Without compression (bypass)
curl -X POST "$GATEWAY/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -H "X-Headroom-Bypass: true" \
  -d '{"model":"claude-opus-4-6","max_tokens":200,"messages":[...]}'
```

See `eval-headroom.sh` for an automated A/B evaluation script.

## When Does Compression Actually Happen?

Headroom's Kompress ML compressor is designed for **large contexts** — it compresses
tool outputs, RAG document chunks, log data, and long conversation histories. Short
prompts (<500 tokens) typically show "no compression savings" because there's nothing
worth compressing.

To see actual compression, send requests with:
- Long system prompts with detailed instructions
- Multi-turn conversations with 10+ messages
- Tool/function call results with verbose output
- RAG context with multiple document chunks
- Large code blocks or log dumps

## Known Issues

- **ext-proc response processing disabled**: The EnvoyFilter must set
  `response_body_mode: NONE` for external providers (Anthropic, OpenAI).
  This means the headroom ResponseProcessor (which adds `X-Headroom-Tokens-Saved`
  response headers) doesn't run. Compression stats are available in BBR logs and
  CycleState only.

- **Kuadrant operator may revert EnvoyFilter**: Re-apply the EnvoyFilter patch
  (response_body_mode: NONE) if requests start failing.

- **`[transformers] PyTorch was not found`**: This warning is benign. The proxy
  uses onnxruntime for inference, not PyTorch. Install `headroom-ai[ml]` only if
  you need PyTorch-based models.

## Verifying Savings

1. **BBR pod logs**: `headroom compression applied tokensBefore=X tokensAfter=Y tokensSaved=Z`
   (or `headroom: no compression savings` for small contexts)
2. **Headroom sidecar logs**: `Transform content_router: X -> Y tokens (saved Z)`
3. **Metering service**: Compare token counts with and without headroom enabled
