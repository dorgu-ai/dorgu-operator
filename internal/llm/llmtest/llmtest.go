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

// Package llmtest is a counting test double for llm.Client, shared by the tests
// of every package that talks to a model.
package llmtest

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/dorgu-ai/dorgu-operator/internal/llm"
)

// Client is a scripted llm.Client that records every request.
type Client struct {
	// Name is the provider name reported; "stub" when empty.
	Name string

	mu       sync.Mutex
	reply    func(llm.Request) (*llm.Response, error)
	requests []llm.Request
}

// New creates a Client that answers every request with reply.
func New(reply func(llm.Request) (*llm.Response, error)) *Client {
	return &Client{reply: reply}
}

// Diagnosing creates a Client that answers every request with a valid diagnosis.
func Diagnosing(summary, rootCause, action string, confidence float64) *Client {
	return New(func(llm.Request) (*llm.Response, error) {
		return DiagnosisReply(summary, rootCause, action, confidence), nil
	})
}

// Failing creates a Client whose every request fails with err.
func Failing(err error) *Client {
	return New(func(llm.Request) (*llm.Response, error) { return nil, err })
}

// Provider returns the configured provider name.
func (c *Client) Provider() string {
	if c.Name == "" {
		return "stub"
	}
	return c.Name
}

// Complete records the request and returns the scripted reply.
func (c *Client) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	reply := c.reply
	c.mu.Unlock()
	return reply(req)
}

// Calls returns how many requests were made.
func (c *Client) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// CallsFor returns how many requests carried the given purpose.
func (c *Client) CallsFor(purpose string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.requests {
		if r.Purpose == purpose {
			n++
		}
	}
	return n
}

// Requests returns a copy of the requests made so far.
func (c *Client) Requests() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]llm.Request(nil), c.requests...)
}

// DiagnosisReply builds a structured diagnosis reply.
func DiagnosisReply(summary, rootCause, action string, confidence float64) *llm.Response {
	raw, _ := json.Marshal(map[string]any{
		"summary":           summary,
		"rootCause":         rootCause,
		"recommendedAction": action,
		"confidence":        confidence,
	})
	return &llm.Response{JSON: raw, Usage: llm.Usage{InputTokens: 100, OutputTokens: 40}}
}

// TextReply builds a reply with prose and no structured output, which is what a
// model that ignores the schema produces.
func TextReply(text string) *llm.Response {
	return &llm.Response{Text: text, Usage: llm.Usage{InputTokens: 100, OutputTokens: 40}}
}
