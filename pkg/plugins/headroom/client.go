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
	compressRawPath      = "/v1/compress-raw"
	defaultTimeoutSec    = 10
	maxResponseBytes     = 10 << 20 // 10 MiB
)

type compressRawRequest struct {
	Texts []string `json:"texts"`
}

type compressRawResultItem struct {
	Compressed       string `json:"compressed"`
	OriginalTokens   int    `json:"original_tokens"`
	CompressedTokens int    `json:"compressed_tokens"`
}

type compressRawResponse struct {
	Results []compressRawResultItem `json:"results"`
}

type headroomClient struct {
	headroomURL    string
	rawURL         string
	httpClient     *http.Client
	config         map[string]any
}

func newHeadroomClient(headroomURL string, rawURL string, timeoutSeconds int, compressConfig map[string]any) (*headroomClient, error) {
	if headroomURL == "" {
		return nil, errors.New("headroomURL is required")
	}
	if rawURL == "" {
		rawURL = headroomURL
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutSec * time.Second
	}

	return &headroomClient{
		headroomURL: headroomURL,
		rawURL:      rawURL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		config: compressConfig,
	}, nil
}

func (c *headroomClient) compressRaw(ctx context.Context, texts []string) ([]compressRawResultItem, error) {
	reqBody := compressRawRequest{Texts: texts}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal compress-raw request: %w", err)
	}

	url := c.rawURL + compressRawPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create compress-raw request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("headroom compress-raw call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("headroom compress-raw returned status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxResponseBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read compress-raw response: %w", err)
	}

	var result compressRawResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode compress-raw response: %w", err)
	}

	return result.Results, nil
}
