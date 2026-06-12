# Headroom Context Compression Plugin

Headroom compresses LLM context (tool outputs, RAG chunks, conversation history)
before requests reach the provider. Same answers, fewer tokens.

Upstream: https://github.com/chopratejas/headroom

## How It Works

The headroom plugin sits in the BBR pipeline after model-provider-resolver and before
api-translation. On each request:

1. Extracts the `messages` array from the request body
2. Sends them to the headroom sidecar (`POST /v1/compress`) for compression
3. Replaces the messages with the compressed version
4. Writes compression stats to CycleState (for metering integration)
5. On the response, adds `X-Headroom-Tokens-Saved` and `X-Headroom-Compression-Ratio` headers

## Deployment

Deploy the headroom sidecar alongside BBR:

```bash
helm install payload-processing ./deploy/payload-processing \
  -f deploy/examples/headroom/values-with-headroom.yaml
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
curl -X POST "$GATEWAY/v1/messages" \
  -H "Content-Type: application/json" \
  -H "x-api-key: $API_KEY" \
  -d '{"model":"my-model","max_tokens":200,"messages":[...]}'

# Without compression (bypass)
curl -X POST "$GATEWAY/v1/messages" \
  -H "Content-Type: application/json" \
  -H "x-api-key: $API_KEY" \
  -H "X-Headroom-Bypass: true" \
  -d '{"model":"my-model","max_tokens":200,"messages":[...]}'
```

Compare the response quality and check `X-Headroom-Tokens-Saved` in the compressed response.

See `eval-headroom.sh` for an automated A/B evaluation script.

## Verifying Savings

Three ways to see compression savings:

1. **Response headers**: `X-Headroom-Tokens-Saved` and `X-Headroom-Compression-Ratio`
2. **BBR pod logs**: `headroom compression applied tokensBefore=X tokensAfter=Y tokensSaved=Z`
3. **Metering service**: Compare token counts with and without headroom enabled
