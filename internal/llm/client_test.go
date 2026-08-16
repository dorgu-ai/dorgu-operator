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
	"testing"
)

func TestNewClient(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		apiKey   string
		wantProv string
		wantErr  bool
	}{
		{
			name:     "claude provider",
			provider: providerClaude,
			apiKey:   "sk-test-key",
			wantProv: providerClaude,
		},
		{
			name:     "gemini provider",
			provider: providerGemini,
			apiKey:   "test-key",
			wantProv: providerGemini,
		},
		{
			name:     "unknown provider",
			provider: "openai",
			apiKey:   "test-key",
			wantErr:  true,
		},
		{
			name:     "empty API key",
			provider: providerClaude,
			apiKey:   "",
			wantErr:  true,
		},
		{
			name:     "empty provider with key",
			provider: "",
			apiKey:   "test-key",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(tt.provider, tt.apiKey)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if client.Provider() != tt.wantProv {
				t.Errorf("provider = %q, want %q", client.Provider(), tt.wantProv)
			}
		})
	}
}
