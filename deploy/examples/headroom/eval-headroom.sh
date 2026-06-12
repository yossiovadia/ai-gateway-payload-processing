#!/bin/bash
# A/B evaluation: same prompt, with and without headroom compression.
# Compares response quality and token savings side by side.
#
# Usage:
#   export GATEWAY="https://maas.apps.example.com/llm/my-model"
#   export API_KEY="sk-oai-..."
#   ./eval-headroom.sh
#
# Or with a custom prompt file (one JSON prompt per line):
#   ./eval-headroom.sh prompts.jsonl

set -euo pipefail

: "${GATEWAY:?Set GATEWAY to your gateway URL (e.g., https://maas.apps.example.com/llm/ext-claude-sonnet)}"
: "${API_KEY:?Set API_KEY to your MaaS API key}"

PROMPT_FILE="${1:-}"
ANTHROPIC_VERSION="2023-06-01"

send_request() {
    local bypass="${1:-false}"
    local prompt="$2"
    local bypass_header=""
    if [ "$bypass" = "true" ]; then
        bypass_header="-H X-Headroom-Bypass:true"
    fi

    # shellcheck disable=SC2086
    curl -sk -X POST "${GATEWAY}/v1/messages" \
        -H "Content-Type: application/json" \
        -H "x-api-key: ${API_KEY}" \
        -H "anthropic-version: ${ANTHROPIC_VERSION}" \
        $bypass_header \
        -d "$prompt" 2>/dev/null
}

compare_single() {
    local prompt="$1"
    local id="$2"

    echo "--- Prompt #${id} ---"

    echo "  [compressed] sending..."
    local resp_a
    resp_a=$(send_request "false" "$prompt")

    echo "  [original]   sending..."
    local resp_b
    resp_b=$(send_request "true" "$prompt")

    local tokens_a tokens_b text_a text_b
    tokens_a=$(echo "$resp_a" | jq -r '.usage.input_tokens // "N/A"')
    tokens_b=$(echo "$resp_b" | jq -r '.usage.input_tokens // "N/A"')
    text_a=$(echo "$resp_a" | jq -r '.content[0].text // .choices[0].message.content // "ERROR"' | head -c 120)
    text_b=$(echo "$resp_b" | jq -r '.content[0].text // .choices[0].message.content // "ERROR"' | head -c 120)

    echo "  Compressed input tokens: ${tokens_a}"
    echo "  Original input tokens:   ${tokens_b}"

    if [ "$tokens_a" != "N/A" ] && [ "$tokens_b" != "N/A" ] && [ "$tokens_b" -gt 0 ] 2>/dev/null; then
        local saved=$((tokens_b - tokens_a))
        local pct
        pct=$(echo "scale=1; ${saved} * 100 / ${tokens_b}" | bc 2>/dev/null || echo "N/A")
        echo "  Tokens saved:            ${saved} (${pct}%)"
    fi

    echo "  [compressed] response: ${text_a}..."
    echo "  [original]   response: ${text_b}..."
    echo ""
}

# Default test prompt
DEFAULT_PROMPT='{"model":"ext-claude-sonnet","max_tokens":100,"messages":[{"role":"user","content":"Explain what Kubernetes is in two sentences."}]}'

if [ -n "$PROMPT_FILE" ] && [ -f "$PROMPT_FILE" ]; then
    echo "=== Headroom A/B Evaluation (from ${PROMPT_FILE}) ==="
    echo ""
    id=1
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        compare_single "$line" "$id"
        id=$((id + 1))
    done < "$PROMPT_FILE"
else
    echo "=== Headroom A/B Evaluation (single prompt) ==="
    echo ""
    compare_single "$DEFAULT_PROMPT" "1"
fi

echo "=== Done ==="
