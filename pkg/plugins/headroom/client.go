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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	compressPath         = "/v1/compress"
	defaultTimeoutSec    = 10
	maxResponseBytes     = 10 << 20 // 10 MiB — compressed messages can be large
)

type compressRequest struct {
	Messages []any          `json:"messages"`
	Model    string         `json:"model"`
	Config   map[string]any `json:"config,omitempty"`
}

type compressResult struct {
	Messages         []any    `json:"messages"`
	TokensBefore     int      `json:"tokens_before"`
	TokensAfter      int      `json:"tokens_after"`
	TokensSaved      int      `json:"tokens_saved"`
	CompressionRatio float64  `json:"compression_ratio"`
	TransformsApplied []string `json:"transforms_applied,omitempty"`
}

type headroomClient struct {
	headroomURL string
	httpClient  *http.Client
	config      map[string]any
}

func newHeadroomClient(headroomURL string, timeoutSeconds int, compressConfig map[string]any) (*headroomClient, error) {
	if headroomURL == "" {
		return nil, errors.New("headroomURL is required")
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutSec * time.Second
	}

	return &headroomClient{
		headroomURL: headroomURL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		config: compressConfig,
	}, nil
}

func (c *headroomClient) compress(ctx context.Context, messages []any, model string) (*compressResult, error) {
	reqBody := compressRequest{
		Messages: messages,
		Model:    model,
		Config:   c.config,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal compress request: %w", err)
	}

	url := c.headroomURL + compressPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create compress request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("headroom compress call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("headroom returned status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxResponseBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read headroom response: %w", err)
	}

	var result compressResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode headroom response: %w", err)
	}

	return &result, nil
}
