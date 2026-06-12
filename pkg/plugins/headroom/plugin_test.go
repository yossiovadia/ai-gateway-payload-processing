/*
Copyright 2026 The opendatahub.io Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package headroom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	errcommon "sigs.k8s.io/gateway-api-inference-extension/pkg/common/error"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/state"
)

func compressRawHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req compressRawRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var results []compressRawResultItem
		for _, text := range req.Texts {
			compressed := text[:len(text)/3] + "..." // simulate ~67% compression
			results = append(results, compressRawResultItem{
				Compressed:       compressed,
				OriginalTokens:   len(text) / 4, // rough token estimate
				CompressedTokens: len(text) / 12,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(compressRawResponse{Results: results})
	}
}

func noSavingsRawHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req compressRawRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var results []compressRawResultItem
		for _, text := range req.Texts {
			results = append(results, compressRawResultItem{
				Compressed:       text,
				OriginalTokens:   100,
				CompressedTokens: 100,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(compressRawResponse{Results: results})
	}
}

func newTestPlugin(t *testing.T, rawURL string) *HeadroomPlugin {
	t.Helper()
	p, err := NewHeadroomPlugin("http://unused:8787", rawURL, 10, true, nil, 2, 500)
	require.NoError(t, err)
	return p
}

func largeToolContent() string {
	return strings.Repeat("log line with lots of data about requests and responses ", 50)
}

func buildAgentConversation(toolResultCount, recentTurns int) []any {
	var messages []any
	messages = append(messages, map[string]any{"role": "system", "content": "You are helpful."})

	for i := 0; i < toolResultCount; i++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": "do something"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "tool", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": largeToolContent()},
			map[string]any{"role": "assistant", "content": "done"},
		)
	}

	for i := 0; i < recentTurns; i++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": "follow up"},
			map[string]any{"role": "assistant", "content": "response"},
		)
	}

	messages = append(messages, map[string]any{"role": "user", "content": "final question"})
	return messages
}

// --- Construction ---

func TestNewHeadroomPlugin(t *testing.T) {
	_, err := NewHeadroomPlugin("http://localhost:8787", "", 10, true, nil, 2, 500)
	require.NoError(t, err)

	_, err = NewHeadroomPlugin("", "", 10, true, nil, 2, 500)
	require.Error(t, err)
}

func TestHeadroomTypedName(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:8787", "", 10, true, nil, 2, 500)
	require.NoError(t, err)
	assert.Equal(t, HeadroomPluginType, p.TypedName().Type)

	p.WithName("my-headroom")
	assert.Equal(t, "my-headroom", p.TypedName().Name)
}

// --- findCompressibleToolResults ---

func TestFindCompressible_OldToolResultsFound(t *testing.T) {
	p := newTestPlugin(t, "http://unused")
	// 3 tool results, 2 recent turns → first tool result is old and compressible
	messages := buildAgentConversation(3, 2)
	candidates := p.findCompressibleToolResults(messages)
	assert.GreaterOrEqual(t, len(candidates), 1, "should find at least 1 old tool result")
}

func TestFindCompressible_RecentToolResultsProtected(t *testing.T) {
	p := newTestPlugin(t, "http://unused")
	// 1 tool result, 0 recent turns → tool result is within last 2 turns, protected
	messages := buildAgentConversation(1, 0)
	candidates := p.findCompressibleToolResults(messages)
	assert.Empty(t, candidates, "tool result in recent turns should be protected")
}

func TestFindCompressible_SmallToolResultsSkipped(t *testing.T) {
	p := newTestPlugin(t, "http://unused")
	messages := []any{
		map[string]any{"role": "user", "content": "do something"},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "small"},
		map[string]any{"role": "user", "content": "next"},
		map[string]any{"role": "user", "content": "next"},
		map[string]any{"role": "user", "content": "final"},
	}
	candidates := p.findCompressibleToolResults(messages)
	assert.Empty(t, candidates, "small tool results should be skipped")
}

func TestFindCompressible_NonToolMessagesIgnored(t *testing.T) {
	p := newTestPlugin(t, "http://unused")
	messages := []any{
		map[string]any{"role": "user", "content": largeToolContent()},
		map[string]any{"role": "assistant", "content": largeToolContent()},
		map[string]any{"role": "system", "content": largeToolContent()},
		map[string]any{"role": "user", "content": "q1"},
		map[string]any{"role": "user", "content": "q2"},
		map[string]any{"role": "user", "content": "final"},
	}
	candidates := p.findCompressibleToolResults(messages)
	assert.Empty(t, candidates, "non-tool messages should never be compressed")
}

// --- ProcessRequest ---

func TestProcessRequest_CompressesOldToolResults(t *testing.T) {
	srv := httptest.NewServer(compressRawHandler())
	defer srv.Close()

	p := newTestPlugin(t, srv.URL)
	messages := buildAgentConversation(3, 2)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = messages

	cs := framework.NewCycleState()
	err := p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	saved, err := framework.ReadCycleStateKey[int](cs, state.HeadroomTokensSavedKey)
	require.NoError(t, err)
	assert.Greater(t, saved, 0)
}

func TestProcessRequest_BypassHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("sidecar should not be called when bypass is set")
	}))
	defer srv.Close()

	p := newTestPlugin(t, srv.URL)

	req := framework.NewInferenceRequest()
	req.Headers[bypassHeader] = "true"
	req.Body["messages"] = buildAgentConversation(3, 2)

	err := p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err)
}

func TestProcessRequest_NoMessages(t *testing.T) {
	p := newTestPlugin(t, "http://unreachable:9999")

	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"

	err := p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err)
}

func TestProcessRequest_NoCompressibleContent(t *testing.T) {
	p := newTestPlugin(t, "http://unreachable:9999")

	req := framework.NewInferenceRequest()
	req.Body["messages"] = []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi"},
	}

	err := p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err)
}

func TestProcessRequest_ServiceDown_FailOpen(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:8787", "http://localhost:1", 1, true, nil, 2, 500)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["messages"] = buildAgentConversation(3, 2)

	err = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err, "fail-open should not return error")
}

func TestProcessRequest_ServiceDown_FailClosed(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:8787", "http://localhost:1", 1, false, nil, 2, 500)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["messages"] = buildAgentConversation(3, 2)

	err = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	require.Error(t, err)

	var infErr errcommon.Error
	require.ErrorAs(t, err, &infErr)
	assert.Equal(t, errcommon.ServiceUnavailable, infErr.Code)
}

func TestProcessRequest_NoSavings(t *testing.T) {
	srv := httptest.NewServer(noSavingsRawHandler())
	defer srv.Close()

	p := newTestPlugin(t, srv.URL)

	req := framework.NewInferenceRequest()
	req.Body["messages"] = buildAgentConversation(3, 2)

	cs := framework.NewCycleState()
	err := p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	_, readErr := framework.ReadCycleStateKey[int](cs, state.HeadroomTokensSavedKey)
	assert.Error(t, readErr, "no stats should be written when no savings")
}

// --- ProcessResponse ---

func TestProcessResponse_AddsHeaders(t *testing.T) {
	p := newTestPlugin(t, "http://unused")

	cs := framework.NewCycleState()
	cs.Write(state.HeadroomTokensBeforeKey, 1000)
	cs.Write(state.HeadroomTokensAfterKey, 400)
	cs.Write(state.HeadroomTokensSavedKey, 600)

	resp := framework.NewInferenceResponse()
	err := p.ProcessResponse(context.Background(), cs, resp)
	require.NoError(t, err)

	assert.Equal(t, "600", resp.Headers[responseTokensSavedHeader])
	assert.Equal(t, "0.60", resp.Headers[responseRatioHeader])
}

func TestProcessResponse_NoStatsSkipsHeaders(t *testing.T) {
	p := newTestPlugin(t, "http://unused")

	resp := framework.NewInferenceResponse()
	err := p.ProcessResponse(context.Background(), framework.NewCycleState(), resp)
	require.NoError(t, err)

	assert.Empty(t, resp.MutatedHeaders())
}

// --- Factory ---

func TestHeadroomFactory(t *testing.T) {
	srv := httptest.NewServer(compressRawHandler())
	defer srv.Close()

	params := json.RawMessage(`{"headroomURL":"` + srv.URL + `","rawURL":"` + srv.URL + `","timeoutSeconds":5,"failOpen":false}`)
	p, err := HeadroomFactory("my-headroom", params, nil)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "my-headroom", p.TypedName().Name)
}

func TestHeadroomFactory_DefaultConfig(t *testing.T) {
	params := json.RawMessage(`{"headroomURL":"http://localhost:8787"}`)
	p, err := HeadroomFactory("test", params, nil)
	require.NoError(t, err)

	hp := p.(*HeadroomPlugin)
	assert.True(t, hp.failOpen)
	assert.Equal(t, defaultProtectRecentTurns, hp.protectRecentTurns)
	assert.Equal(t, defaultMinCompressChars, hp.minCompressChars)
}

func TestHeadroomFactory_MissingURL(t *testing.T) {
	_, err := HeadroomFactory("test", json.RawMessage(`{}`), nil)
	require.Error(t, err)
}

func TestHeadroomFactory_InvalidJSON(t *testing.T) {
	_, err := HeadroomFactory("test", json.RawMessage(`{invalid`), nil)
	require.Error(t, err)
}

func TestHeadroomFactory_CustomTurnsAndChars(t *testing.T) {
	params := json.RawMessage(`{"headroomURL":"http://localhost:8787","protectRecentTurns":5,"minCompressChars":1000}`)
	p, err := HeadroomFactory("test", params, nil)
	require.NoError(t, err)

	hp := p.(*HeadroomPlugin)
	assert.Equal(t, 5, hp.protectRecentTurns)
	assert.Equal(t, 1000, hp.minCompressChars)
}
