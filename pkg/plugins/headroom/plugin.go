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
	"fmt"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	errcommon "sigs.k8s.io/gateway-api-inference-extension/pkg/common/error"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/common/observability/logging"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/state"
)

const (
	HeadroomPluginType = "headroom"

	bypassHeader              = "x-headroom-bypass"
	responseTokensSavedHeader = "x-headroom-tokens-saved"
	responseRatioHeader       = "x-headroom-compression-ratio"
)

var (
	_ framework.RequestProcessor  = &HeadroomPlugin{}
	_ framework.ResponseProcessor = &HeadroomPlugin{}
)

type headroomConfig struct {
	HeadroomURL    string         `json:"headroomURL"`
	TimeoutSeconds int            `json:"timeoutSeconds"`
	FailOpen       *bool          `json:"failOpen"`
	CompressConfig map[string]any `json:"compressConfig"`
}

func (c *headroomConfig) isFailOpen() bool {
	return c.FailOpen == nil || *c.FailOpen
}

type HeadroomPlugin struct {
	typedName plugin.TypedName
	client    *headroomClient
	failOpen  bool
}

func HeadroomFactory(name string, rawParameters json.RawMessage, _ framework.Handle) (framework.BBRPlugin, error) {
	config := headroomConfig{
		TimeoutSeconds: defaultTimeoutSec,
	}

	if len(rawParameters) > 0 {
		if err := json.Unmarshal(rawParameters, &config); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of '%s' plugin - %w", HeadroomPluginType, err)
		}
	}

	p, err := NewHeadroomPlugin(config.HeadroomURL, config.TimeoutSeconds, config.isFailOpen(), config.CompressConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create '%s' plugin - %w", HeadroomPluginType, err)
	}

	return p.WithName(name), nil
}

func NewHeadroomPlugin(headroomURL string, timeoutSeconds int, failOpen bool, compressConfig map[string]any) (*HeadroomPlugin, error) {
	client, err := newHeadroomClient(headroomURL, timeoutSeconds, compressConfig)
	if err != nil {
		return nil, err
	}
	return &HeadroomPlugin{
		typedName: plugin.TypedName{Type: HeadroomPluginType, Name: HeadroomPluginType},
		client:    client,
		failOpen:  failOpen,
	}, nil
}

func (p *HeadroomPlugin) TypedName() plugin.TypedName {
	return p.typedName
}

func (p *HeadroomPlugin) WithName(name string) *HeadroomPlugin {
	p.typedName.Name = name
	return p
}

func (p *HeadroomPlugin) ProcessRequest(ctx context.Context, cycleState *framework.CycleState, request *framework.InferenceRequest) error {
	logger := log.FromContext(ctx).V(logutil.DEFAULT)

	if request.Headers[bypassHeader] == "true" {
		logger.Info("headroom bypass requested, skipping compression")
		return nil
	}

	rawMessages, ok := request.Body["messages"]
	if !ok {
		return nil
	}
	messages, ok := rawMessages.([]any)
	if !ok || len(messages) == 0 {
		return nil
	}

	model := p.resolveModel(cycleState, request)

	result, err := p.client.compress(ctx, messages, model)
	if err != nil {
		if p.failOpen {
			logger.Error(err, "headroom compression failed, passing through uncompressed (fail-open)")
			return nil
		}
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("headroom compression failed: %v", err)}
	}

	if result.TokensSaved <= 0 {
		logger.Info("headroom: no compression savings, using original messages")
		return nil
	}

	request.SetBodyField("messages", result.Messages)

	cycleState.Write(state.HeadroomTokensBeforeKey, result.TokensBefore)
	cycleState.Write(state.HeadroomTokensAfterKey, result.TokensAfter)
	cycleState.Write(state.HeadroomTokensSavedKey, result.TokensSaved)

	logger.Info("headroom compression applied",
		"tokensBefore", result.TokensBefore,
		"tokensAfter", result.TokensAfter,
		"tokensSaved", result.TokensSaved,
		"compressionRatio", result.CompressionRatio,
	)

	return nil
}

func (p *HeadroomPlugin) ProcessResponse(ctx context.Context, cycleState *framework.CycleState, response *framework.InferenceResponse) error {
	saved, err := framework.ReadCycleStateKey[int](cycleState, state.HeadroomTokensSavedKey)
	if err != nil {
		return nil
	}

	before, _ := framework.ReadCycleStateKey[int](cycleState, state.HeadroomTokensBeforeKey)

	response.SetHeader(responseTokensSavedHeader, strconv.Itoa(saved))
	if before > 0 {
		ratio := float64(saved) / float64(before)
		response.SetHeader(responseRatioHeader, strconv.FormatFloat(ratio, 'f', 2, 64))
	}

	log.FromContext(ctx).V(logutil.DEFAULT).Info("headroom savings reported in response headers",
		"tokensSaved", saved,
		"tokensBefore", before,
	)

	return nil
}

func (p *HeadroomPlugin) resolveModel(cycleState *framework.CycleState, request *framework.InferenceRequest) string {
	if model, err := framework.ReadCycleStateKey[string](cycleState, state.ModelKey); err == nil && model != "" {
		return model
	}
	if model, ok := request.Body["model"].(string); ok {
		return model
	}
	return ""
}
