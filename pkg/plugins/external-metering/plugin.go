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

package external_metering

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	errcommon "sigs.k8s.io/gateway-api-inference-extension/pkg/common/error"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/common/observability/logging"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/state"
)

const (
	ExternalMeteringPluginType = "external-metering"

	defaultTimeoutSec = 5
	defaultFeatureKey = "inference-tokens"
	defaultSource     = "maas-gateway"

	userHeader         = "x-maas-username"
	groupHeader        = "x-maas-group"
	subscriptionHeader = "x-maas-subscription"
)

var _ framework.RequestProcessor = &ExternalMeteringPlugin{}
var _ framework.ResponseProcessor = &ExternalMeteringPlugin{}

type externalMeteringConfig struct {
	MeteringURL    string `json:"meteringURL"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	FeatureKey     string `json:"featureKey,omitempty"`
	Source         string `json:"source,omitempty"`
	FailOpen       *bool  `json:"failOpen,omitempty"`
}

type ExternalMeteringPlugin struct {
	typedName  plugin.TypedName
	client     *meteringClient
	featureKey string
	source     string
	failOpen   bool
}

func ExternalMeteringFactory(name string, rawParameters json.RawMessage, _ framework.Handle) (framework.BBRPlugin, error) {
	defaultFailOpen := true
	config := externalMeteringConfig{
		TimeoutSeconds: defaultTimeoutSec,
		FeatureKey:     defaultFeatureKey,
		Source:         defaultSource,
		FailOpen:       &defaultFailOpen,
	}

	if len(rawParameters) > 0 {
		if err := json.Unmarshal(rawParameters, &config); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of '%s' plugin - %w", ExternalMeteringPluginType, err)
		}
	}

	if config.MeteringURL == "" {
		return nil, fmt.Errorf("'meteringURL' is required for '%s' plugin", ExternalMeteringPluginType)
	}

	if config.FeatureKey == "" {
		config.FeatureKey = defaultFeatureKey
	}
	if config.Source == "" {
		config.Source = defaultSource
	}

	p := &ExternalMeteringPlugin{
		typedName: plugin.TypedName{
			Type: ExternalMeteringPluginType,
			Name: ExternalMeteringPluginType,
		},
		client:     newMeteringClient(config.MeteringURL, config.TimeoutSeconds),
		featureKey: config.FeatureKey,
		source:     config.Source,
		failOpen:   config.FailOpen == nil || *config.FailOpen,
	}

	return p.WithName(name), nil
}

func (p *ExternalMeteringPlugin) TypedName() plugin.TypedName {
	return p.typedName
}

func (p *ExternalMeteringPlugin) WithName(name string) *ExternalMeteringPlugin {
	p.typedName.Name = name
	return p
}

func (p *ExternalMeteringPlugin) ProcessRequest(ctx context.Context, cycleState *framework.CycleState, request *framework.InferenceRequest) error {
	logger := log.FromContext(ctx)

	username := request.Headers[userHeader]
	if username == "" {
		username = request.Headers[subscriptionHeader]
	}
	if username == "" {
		logger.V(logutil.VERBOSE).Info("no username or subscription header found, skipping metering")
		return nil
	}

	subscription := request.Headers[subscriptionHeader]
	group := request.Headers[groupHeader]
	if group == "" {
		group = subscription
	}

	model, _ := request.Body["model"].(string)

	cycleState.Write(state.MeteringUsernameKey, username)
	cycleState.Write(state.MeteringGroupKey, group)
	cycleState.Write(state.MeteringSubscriptionKey, subscription)
	cycleState.Write(state.MeteringModelKey, model)
	cycleState.Write(state.MeteringRequestTimeKey, time.Now())

	if streaming, ok := request.Body["stream"].(bool); ok && streaming {
		opts, _ := request.Body["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		request.Body["stream_options"] = opts
	}

	result, err := p.client.checkBalance(ctx, username, p.featureKey, model)
	if err != nil {
		if p.failOpen {
			logger.Error(err, "metering balance check failed (fail-open), allowing request")
			return nil
		}
		return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: fmt.Sprintf("metering system unavailable: %v", err)}
	}

	if !result.HasAccess {
		logger.Info("request blocked by metering", "customer", username, "balance", result.Balance)
		return errcommon.Error{Code: errcommon.ResourceExhausted, Msg: "token budget exhausted"}
	}

	logger.V(logutil.VERBOSE).Info("metering check passed", "customer", username, "balance", result.Balance)

	return nil
}

func (p *ExternalMeteringPlugin) ProcessResponse(ctx context.Context, cycleState *framework.CycleState, response *framework.InferenceResponse) error {
	logger := log.FromContext(ctx)

	username, err := framework.ReadCycleStateKey[string](cycleState, state.MeteringUsernameKey)
	if err != nil || username == "" {
		return nil
	}

	group, _ := framework.ReadCycleStateKey[string](cycleState, state.MeteringGroupKey)
	subscription, _ := framework.ReadCycleStateKey[string](cycleState, state.MeteringSubscriptionKey)
	model, _ := framework.ReadCycleStateKey[string](cycleState, state.MeteringModelKey)
	provider, _ := framework.ReadCycleStateKey[string](cycleState, state.ProviderKey)

	usage, ok := response.Body["usage"].(map[string]any)
	if !ok {
		logger.V(logutil.VERBOSE).Info("no usage data in response, skipping metering report")
		return nil
	}

	promptTokens, completionTokens, totalTokens := extractTokenCounts(usage)

	// Extract detailed token breakdown
	cachedInputTokens := 0
	cacheCreationTokens := 0
	reasoningTokens := 0

	// Anthropic format: cache_read_input_tokens, cache_creation_input_tokens
	if v := toInt(usage["cache_read_input_tokens"]); v > 0 {
		cachedInputTokens = v
	}
	if v := toInt(usage["cache_creation_input_tokens"]); v > 0 {
		cacheCreationTokens = v
	}

	// OpenAI format: prompt_tokens_details.cached_tokens, completion_tokens_details.reasoning_tokens
	// Only read OpenAI fields if Anthropic fields were not present (avoid overwrite)
	if cachedInputTokens == 0 {
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			cachedInputTokens = toInt(details["cached_tokens"])
		}
	}
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		reasoningTokens = toInt(details["reasoning_tokens"])
	}

	event := map[string]any{
		"specversion":     "1.0",
		"id":              fmt.Sprintf("evt-%s", uuid.New().String()),
		"source":          p.source,
		"type":            "inference.tokens.used",
		"subject":         username,
		"time":            time.Now().UTC().Format(time.RFC3339),
		"datacontenttype": "application/json",
		"data": map[string]any{
			"user":                username,
			"group":               group,
			"subscription":        subscription,
			"provider":            provider,
			"model":               model,
			"prompt_tokens":       promptTokens,
			"completion_tokens":   completionTokens,
			"total_tokens":        totalTokens,
			"cached_input_tokens": cachedInputTokens,
			"cache_creation_tokens": cacheCreationTokens,
			"reasoning_tokens":    reasoningTokens,
		},
	}

	eventJSON, marshalErr := json.Marshal(event)
	if marshalErr != nil {
		logger.Error(marshalErr, "failed to marshal usage event")
		return nil
	}

	if reportErr := p.client.reportUsage(ctx, eventJSON); reportErr != nil {
		logger.Error(reportErr, "failed to report usage to metering system")
	} else {
		logger.V(logutil.VERBOSE).Info("usage reported", "customer", username, "model", model, "tokens", totalTokens)
	}

	return nil
}

// extractTokenCounts normalizes token usage from different provider response formats.
// Runs before api-translation, so must handle native provider formats:
//   - OpenAI/Azure/Bedrock: prompt_tokens, completion_tokens, total_tokens
//   - Anthropic: input_tokens, output_tokens
func extractTokenCounts(usage map[string]any) (prompt, completion, total int) {
	prompt = toInt(usage["prompt_tokens"])
	completion = toInt(usage["completion_tokens"])
	total = toInt(usage["total_tokens"])

	if prompt == 0 && completion == 0 {
		prompt = toInt(usage["input_tokens"])
		completion = toInt(usage["output_tokens"])
	}

	if total == 0 {
		total = prompt + completion
	}
	return
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}
