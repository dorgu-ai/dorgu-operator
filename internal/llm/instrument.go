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

package llm

import (
	"context"
	"time"

	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
)

// instrumented counts every request a Client sends. It sits behind the one Client
// interface, so diagnosis and planning are counted by the same code and a new
// provider is counted without writing any metrics.
type instrumented struct {
	inner Client
}

// Instrument wraps c so that each Complete call is recorded in
// dorgu_llm_requests_total (outcome success or error) and, when the provider
// reports usage, dorgu_llm_tokens_total. Wrapping an already instrumented client
// returns it unchanged, so a request is never counted twice.
func Instrument(c Client) Client {
	if _, already := c.(*instrumented); already {
		return c
	}
	return &instrumented{inner: c}
}

// Provider returns the wrapped provider's name.
func (i *instrumented) Provider() string { return i.inner.Provider() }

// Unwrap returns the client being counted.
func (i *instrumented) Unwrap() Client { return i.inner }

// Complete forwards the request and records the outcome.
func (i *instrumented) Complete(ctx context.Context, req Request) (*Response, error) {
	started := time.Now()
	resp, err := i.inner.Complete(ctx, req)
	provider := i.inner.Provider()
	metrics.RecordLLMRequestDuration(provider, req.Purpose, time.Since(started))
	if err != nil {
		metrics.RecordLLMRequest(provider, req.Purpose, metrics.LLMOutcomeError)
		return nil, err
	}
	metrics.RecordLLMRequest(provider, req.Purpose, metrics.LLMOutcomeSuccess)
	metrics.RecordLLMTokens(provider, req.Purpose, metrics.LLMTokensInput, resp.Usage.InputTokens)
	metrics.RecordLLMTokens(provider, req.Purpose, metrics.LLMTokensOutput, resp.Usage.OutputTokens)
	return resp, nil
}
