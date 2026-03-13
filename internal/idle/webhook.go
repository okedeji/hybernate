/*
Copyright 2026.

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

package idle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const webhookTimeout = 5 * time.Second

type webhookResponse struct {
	Idle bool `json:"idle"`
}

type Webhook struct {
	URL    string
	client *http.Client
}

func NewWebhook(url string) *Webhook {
	return &Webhook{
		URL: url,
		client: &http.Client{
			Timeout: webhookTimeout,
		},
	}
}

func (w *Webhook) Check(ctx context.Context, namespace, name string) (Result, error) {
	url := fmt.Sprintf("%s?namespace=%s&name=%s", w.URL, namespace, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, fmt.Errorf("building webhook request: %w", err)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("calling webhook %s: %w", w.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("webhook %s returned status %d", w.URL, resp.StatusCode)
	}

	var body webhookResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Result{}, fmt.Errorf("decoding webhook response: %w", err)
	}

	if body.Idle {
		return Result{Active: false, Reason: "webhook reports idle"}, nil
	}
	return Result{Active: true, Reason: "webhook reports active"}, nil
}
