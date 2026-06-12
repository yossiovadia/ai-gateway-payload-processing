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

func compressHandler(tokensBefore, tokensAfter int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req compressRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		saved := tokensBefore - tokensAfter
		ratio := float64(tokensAfter) / float64(tokensBefore)
		resp := compressResult{
			Messages:         req.Messages[:1], // simulate compression by keeping first message
			TokensBefore:     tokensBefore,
			TokensAfter:      tokensAfter,
			TokensSaved:      saved,
			CompressionRatio: ratio,
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
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "valid config", url: "http://localhost:8787"},
		{name: "missing URL", url: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewHeadroomPlugin(tt.url, 10, true, nil)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, p)
		})
	}
}

func TestHeadroomTypedName(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:8787", 10, true, nil)
	require.NoError(t, err)

	assert.Equal(t, HeadroomPluginType, p.TypedName().Type)
	assert.Equal(t, HeadroomPluginType, p.TypedName().Name)

	p.WithName("my-headroom")
	assert.Equal(t, "my-headroom", p.TypedName().Name)
	assert.Equal(t, HeadroomPluginType, p.TypedName().Type)
}

// --- ProcessRequest ---

func TestProcessRequest_CompressionSucceeds(t *testing.T) {
	srv := httptest.NewServer(compressHandler(1000, 400))
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true, nil)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = []any{
		map[string]any{"role": "user", "content": "Hello world"},
		map[string]any{"role": "assistant", "content": "Hi there"},
		map[string]any{"role": "user", "content": "How are you?"},
	}

	cs := framework.NewCycleState()
	err = p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	// Messages should be replaced with compressed version
	messages, ok := req.Body["messages"].([]any)
	require.True(t, ok)
	assert.Len(t, messages, 1, "compressed messages should replace original")

	// CycleState should have stats
	before, err := framework.ReadCycleStateKey[int](cs, state.HeadroomTokensBeforeKey)
	require.NoError(t, err)
	assert.Equal(t, 1000, before)

	after, err := framework.ReadCycleStateKey[int](cs, state.HeadroomTokensAfterKey)
	require.NoError(t, err)
	assert.Equal(t, 400, after)

	saved, err := framework.ReadCycleStateKey[int](cs, state.HeadroomTokensSavedKey)
	require.NoError(t, err)
	assert.Equal(t, 600, saved)
}

func TestProcessRequest_BypassHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("headroom service should not be called when bypass is set")
	}))
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true, nil)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Headers[bypassHeader] = "true"
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "Hello"}}

	err = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err)
}

func TestProcessRequest_NoMessages(t *testing.T) {
	p, err := NewHeadroomPlugin("http://unreachable:9999", 10, true, nil)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"
	// no messages field

	err = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err)
}

func TestProcessRequest_EmptyMessages(t *testing.T) {
	p, err := NewHeadroomPlugin("http://unreachable:9999", 10, true, nil)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["messages"] = []any{}

	err = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err)
}

func TestProcessRequest_NoSavings(t *testing.T) {
	srv := httptest.NewServer(noSavingsHandler())
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true, nil)
	require.NoError(t, err)

	originalMessages := []any{
		map[string]any{"role": "user", "content": "short"},
	}
	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = originalMessages

	cs := framework.NewCycleState()
	err = p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	// Messages should be unchanged
	messages := req.Body["messages"].([]any)
	assert.Len(t, messages, 1)

	// CycleState should NOT have stats (no savings)
	_, readErr := framework.ReadCycleStateKey[int](cs, state.HeadroomTokensSavedKey)
	assert.Error(t, readErr)
}

func TestProcessRequest_ServiceDown_FailOpen(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:1", 1, true, nil)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "Hello"}}

	err = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err, "fail-open should not return error")

	// Messages should be unchanged
	messages := req.Body["messages"].([]any)
	assert.Len(t, messages, 1)
}

func TestProcessRequest_ServiceDown_FailClosed(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:1", 1, false, nil)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "Hello"}}

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
		resp := compressResult{
			Messages:     req.Messages,
			TokensBefore: 100,
			TokensAfter:  100,
			TokensSaved:  0,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p, err := NewHeadroomPlugin(srv.URL, 10, true, nil)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "client-model"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "Hello"}}

	cs := framework.NewCycleState()
	cs.Write(state.ModelKey, "claude-opus-4-6")

	_ = p.ProcessRequest(context.Background(), cs, req)
	assert.Equal(t, "claude-opus-4-6", capturedModel, "should prefer target model from CycleState")
}

func TestProcessRequest_ForwardsCompressConfig(t *testing.T) {
	var capturedConfig map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compressRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedConfig = req.Config
		resp := compressResult{
			Messages:     req.Messages,
			TokensBefore: 100,
			TokensAfter:  100,
			TokensSaved:  0,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	compressCfg := map[string]any{
		"target_ratio":   0.5,
		"protect_recent": float64(4),
	}
	p, err := NewHeadroomPlugin(srv.URL, 10, true, compressCfg)
	require.NoError(t, err)

	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "Hello"}}

	_ = p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.Equal(t, 0.5, capturedConfig["target_ratio"])
	assert.Equal(t, float64(4), capturedConfig["protect_recent"])
}

// --- ProcessResponse ---

func TestProcessResponse_AddsHeaders(t *testing.T) {
	p, err := NewHeadroomPlugin("http://localhost:8787", 10, true, nil)
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
	p, err := NewHeadroomPlugin("http://localhost:8787", 10, true, nil)
	require.NoError(t, err)

	resp := framework.NewInferenceResponse()
	err = p.ProcessResponse(context.Background(), framework.NewCycleState(), resp)
	require.NoError(t, err)

	assert.Empty(t, resp.MutatedHeaders(), "no headers should be set when no compression stats")
}

// --- Factory ---

func TestHeadroomFactory(t *testing.T) {
	srv := httptest.NewServer(compressHandler(100, 50))
	defer srv.Close()

	params := json.RawMessage(`{"headroomURL":"` + srv.URL + `","timeoutSeconds":5,"failOpen":false}`)
	p, err := HeadroomFactory("my-headroom", params, nil)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "my-headroom", p.TypedName().Name)
	assert.Equal(t, HeadroomPluginType, p.TypedName().Type)
}

func TestHeadroomFactory_DefaultConfig(t *testing.T) {
	srv := httptest.NewServer(compressHandler(100, 50))
	defer srv.Close()

	params := json.RawMessage(`{"headroomURL":"` + srv.URL + `"}`)
	p, err := HeadroomFactory("test", params, nil)
	require.NoError(t, err)

	hp := p.(*HeadroomPlugin)
	assert.True(t, hp.failOpen, "failOpen should default to true")
}

func TestHeadroomFactory_MissingURL(t *testing.T) {
	params := json.RawMessage(`{}`)
	_, err := HeadroomFactory("test", params, nil)
	require.Error(t, err)
}

func TestHeadroomFactory_InvalidJSON(t *testing.T) {
	params := json.RawMessage(`{invalid`)
	_, err := HeadroomFactory("test", params, nil)
	require.Error(t, err)
}
