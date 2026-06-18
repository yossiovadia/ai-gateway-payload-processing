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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	errcommon "sigs.k8s.io/gateway-api-inference-extension/pkg/common/error"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/state"
)

func compressHandler(savedTokens int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req compressRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		before := 1000
		after := before - savedTokens
		resp := compressResult{
			Messages:         req.Messages[:1],
			TokensBefore:     before,
			TokensAfter:      after,
			TokensSaved:      savedTokens,
			CompressionRatio: float64(after) / float64(before),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func noSavingsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req compressRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := compressResult{
			Messages:         req.Messages,
			TokensBefore:     100,
			TokensAfter:      100,
			TokensSaved:      0,
			CompressionRatio: 1.0,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// --- Construction ---

func TestNewHeadroomPlugin(t *testing.T) {
	_, err := NewHeadroomPlugin("http://localhost:8787", 10, true)
	require.NoError(t, err)

	_, err = NewHeadroomPlugin("", 10, true)
	require.Error(t, err)
}

func TestHeadroomTypedName(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:8787", 10, true)
	require.NoError(t, err)
	assert.Equal(t, HeadroomPluginType, p.TypedName().Type)

	p.WithName("my-headroom")
	assert.Equal(t, "my-headroom", p.TypedName().Name)
}

// --- ProcessRequest ---

func TestProcessRequest_CompressionSucceeds(t *testing.T) {
	srv := httptest.NewServer(compressHandler(600))
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "claude-opus-4-8"
	req.Body["messages"] = []any{
		map[string]any{"role": "user", "content": "read file"},
		map[string]any{"role": "assistant", "content": "reading..."},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": "large file content here"},
		}},
	}

	cs := framework.NewCycleState()
	err = p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	saved, err := framework.ReadCycleStateKey[int](cs, state.HeadroomTokensSavedKey)
	require.NoError(t, err)
	assert.Equal(t, 600, saved)
}

func TestProcessRequest_NoSavings(t *testing.T) {
	srv := httptest.NewServer(noSavingsHandler())
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "claude-opus-4-8"
	req.Body["messages"] = []any{
		map[string]any{"role": "user", "content": "hello"},
	}

	cs := framework.NewCycleState()
	err = p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	_, readErr := framework.ReadCycleStateKey[int](cs, state.HeadroomTokensSavedKey)
	assert.Error(t, readErr, "no stats when no savings")
}

func TestProcessRequest_NoMessages(t *testing.T) {
	p, err := NewHeadroomPlugin("http://unreachable:9999", 10, true)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "test"

	err = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err)
}

func TestProcessRequest_ServiceDown_FailOpen(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:1", 1, true)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}

	err = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err)
}

func TestProcessRequest_ServiceDown_FailClosed(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:1", 1, false)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}

	err = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	require.Error(t, err)

	var infErr errcommon.Error
	require.ErrorAs(t, err, &infErr)
	assert.Equal(t, errcommon.ServiceUnavailable, infErr.Code)
}

func TestProcessRequest_UsesModelFromCycleState(t *testing.T) {
	var capturedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compressRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedModel = req.Model
		resp := compressResult{Messages: req.Messages, TokensSaved: 0}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "client-model"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}

	cs := framework.NewCycleState()
	cs.Write(state.ModelKey, "claude-opus-4-8")

	_ = p.ProcessRequest(context.Background(), cs, req)
	assert.Equal(t, "claude-opus-4-8", capturedModel)
}

func TestProcessRequest_SendsFullMessages(t *testing.T) {
	var capturedMessages []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compressRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedMessages = req.Messages
		resp := compressResult{Messages: req.Messages, TokensSaved: 0}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true)
	require.NoError(t, err)

	messages := []any{
		map[string]any{"role": "system", "content": "you are helpful"},
		map[string]any{"role": "user", "content": "read file"},
		map[string]any{"role": "assistant", "content": "reading"},
		map[string]any{"role": "user", "content": "summarize"},
	}
	req := framework.NewInferenceRequest()
	req.Body["model"] = "test"
	req.Body["messages"] = messages

	_ = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.Len(t, capturedMessages, 4, "all messages should be sent to headroom")
}

func TestProcessRequest_ForwardsUsername(t *testing.T) {
	var capturedUsername string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUsername = r.Header.Get("x-maas-username")
		var req compressRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := compressResult{Messages: req.Messages, TokensSaved: 0}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Headers["x-maas-username"] = "yossi"
	req.Body["model"] = "test"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}

	_ = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.Equal(t, "yossi", capturedUsername)
}

func TestProcessRequest_NoUsernameHeader(t *testing.T) {
	var capturedUsername string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUsername = r.Header.Get("x-maas-username")
		var req compressRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := compressResult{Messages: req.Messages, TokensSaved: 0}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "test"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}

	_ = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.Empty(t, capturedUsername, "no username header should mean empty")
}

// --- ProcessResponse ---

func TestProcessResponse_AddsHeaders(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:8787", 10, true)
	require.NoError(t, err)

	cs := framework.NewCycleState()
	cs.Write(state.HeadroomTokensBeforeKey, 1000)
	cs.Write(state.HeadroomTokensAfterKey, 400)
	cs.Write(state.HeadroomTokensSavedKey, 600)

	resp := framework.NewInferenceResponse()
	err = p.ProcessResponse(context.Background(), cs, resp)
	require.NoError(t, err)

	assert.Equal(t, "600", resp.Headers[responseTokensSavedHeader])
	assert.Equal(t, "0.60", resp.Headers[responseRatioHeader])
}

func TestProcessResponse_NoStatsSkipsHeaders(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:8787", 10, true)
	require.NoError(t, err)

	resp := framework.NewInferenceResponse()
	err = p.ProcessResponse(context.Background(), framework.NewCycleState(), resp)
	require.NoError(t, err)

	assert.Empty(t, resp.MutatedHeaders())
}

// --- Factory ---

func TestHeadroomFactory(t *testing.T) {
	srv := httptest.NewServer(compressHandler(100))
	defer srv.Close()

	params := json.RawMessage(`{"headroomURL":"` + srv.URL + `","timeoutSeconds":5,"failOpen":false}`)
	p, err := HeadroomFactory("my-headroom", params, nil)
	require.NoError(t, err)
	assert.Equal(t, "my-headroom", p.TypedName().Name)
}

func TestHeadroomFactory_DefaultConfig(t *testing.T) {
	params := json.RawMessage(`{"headroomURL":"http://localhost:8787"}`)
	p, err := HeadroomFactory("test", params, nil)
	require.NoError(t, err)

	hp := p.(*HeadroomPlugin)
	assert.True(t, hp.failOpen)
}

func TestHeadroomFactory_MissingURL(t *testing.T) {
	_, err := HeadroomFactory("test", json.RawMessage(`{}`), nil)
	require.Error(t, err)
}

func TestHeadroomFactory_InvalidJSON(t *testing.T) {
	_, err := HeadroomFactory("test", json.RawMessage(`{invalid`), nil)
	require.Error(t, err)
}
