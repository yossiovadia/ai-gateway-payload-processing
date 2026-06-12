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

const (
	defaultProtectRecentTurns = 2
	defaultMinCompressChars   = 500
)

type headroomConfig struct {
	HeadroomURL        string         `json:"headroomURL"`
	RawURL             string         `json:"rawURL"`
	TimeoutSeconds     int            `json:"timeoutSeconds"`
	FailOpen           *bool          `json:"failOpen"`
	CompressConfig     map[string]any `json:"compressConfig"`
	ProtectRecentTurns *int           `json:"protectRecentTurns"`
	MinCompressChars   *int           `json:"minCompressChars"`
}

func (c *headroomConfig) isFailOpen() bool {
	return c.FailOpen == nil || *c.FailOpen
}

type HeadroomPlugin struct {
	typedName          plugin.TypedName
	client             *headroomClient
	failOpen           bool
	protectRecentTurns int
	minCompressChars   int
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

	protectTurns := defaultProtectRecentTurns
	if config.ProtectRecentTurns != nil {
		protectTurns = *config.ProtectRecentTurns
	}
	minChars := defaultMinCompressChars
	if config.MinCompressChars != nil {
		minChars = *config.MinCompressChars
	}

	p, err := NewHeadroomPlugin(config.HeadroomURL, config.RawURL, config.TimeoutSeconds, config.isFailOpen(), config.CompressConfig, protectTurns, minChars)
	if err != nil {
		return nil, fmt.Errorf("failed to create '%s' plugin - %w", HeadroomPluginType, err)
	}

	return p.WithName(name), nil
}

func NewHeadroomPlugin(headroomURL, rawURL string, timeoutSeconds int, failOpen bool, compressConfig map[string]any, protectRecentTurns, minCompressChars int) (*HeadroomPlugin, error) {
	client, err := newHeadroomClient(headroomURL, rawURL, timeoutSeconds, compressConfig)
	if err != nil {
		return nil, err
	}
	return &HeadroomPlugin{
		typedName:          plugin.TypedName{Type: HeadroomPluginType, Name: HeadroomPluginType},
		client:             client,
		failOpen:           failOpen,
		protectRecentTurns: protectRecentTurns,
		minCompressChars:   minCompressChars,
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

	// Find compressible tool results: old (beyond protectRecentTurns) and large enough
	candidates := p.findCompressibleToolResults(messages)
	if len(candidates) == 0 {
		logger.Info("headroom: no compressible tool results found")
		return nil
	}

	// Extract text content from candidates
	texts := make([]string, len(candidates))
	for i, c := range candidates {
		texts[i] = c.content
	}

	// Send to sidecar for raw compression
	results, err := p.client.compressRaw(ctx, texts)
	if err != nil {
		if p.failOpen {
			logger.Error(err, "headroom compression failed, passing through uncompressed (fail-open)")
			return nil
		}
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("headroom compression failed: %v", err)}
	}

	// Replace tool content with compressed versions
	totalBefore, totalAfter := 0, 0
	for i, result := range results {
		if i >= len(candidates) {
			break
		}
		if result.CompressedTokens >= result.OriginalTokens {
			continue
		}
		candidates[i].setContent(messages, result.Compressed)
		totalBefore += result.OriginalTokens
		totalAfter += result.CompressedTokens
	}

	totalSaved := totalBefore - totalAfter
	if totalSaved <= 0 {
		logger.Info("headroom: no compression savings after processing")
		return nil
	}

	request.SetBodyField("messages", messages)

	cycleState.Write(state.HeadroomTokensBeforeKey, totalBefore)
	cycleState.Write(state.HeadroomTokensAfterKey, totalAfter)
	cycleState.Write(state.HeadroomTokensSavedKey, totalSaved)

	logger.Info("headroom compression applied",
		"toolResults", len(candidates),
		"tokensBefore", totalBefore,
		"tokensAfter", totalAfter,
		"tokensSaved", totalSaved,
	)

	return nil
}

type toolResultCandidate struct {
	msgIndex int
	content  string
}

func (c *toolResultCandidate) setContent(messages []any, compressed string) {
	msg, ok := messages[c.msgIndex].(map[string]any)
	if !ok {
		return
	}
	msg["content"] = compressed
}

func (p *HeadroomPlugin) findCompressibleToolResults(messages []any) []toolResultCandidate {
	// Count turns from the end (a turn = a user message)
	turnCount := 0
	protectionBoundary := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "user" {
			turnCount++
			if turnCount >= p.protectRecentTurns {
				protectionBoundary = i
				break
			}
		}
	}

	// Find tool messages before the protection boundary
	var candidates []toolResultCandidate
	for i := 0; i < protectionBoundary; i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "tool" {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok || len(content) < p.minCompressChars {
			continue
		}
		candidates = append(candidates, toolResultCandidate{
			msgIndex: i,
			content:  content,
		})
	}

	return candidates
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

