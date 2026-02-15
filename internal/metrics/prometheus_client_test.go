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

package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrometheusClient_GetResourceBaseline(t *testing.T) {
	// Create a mock Prometheus server
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		assert.Equal(t, "/api/v1/query", r.URL.Path)

		response := PrometheusResponse{
			Status: "success",
		}
		response.Data.ResultType = "vector"
		response.Data.Result = []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
			Values [][]interface{}   `json:"values"`
		}{
			{
				Metric: map[string]string{
					"pod":       "my-app-abc123",
					"container": "my-app",
					"namespace": "default",
				},
				Value: []interface{}{float64(time.Now().Unix()), "0.5"},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	baseline, err := client.GetResourceBaseline(context.Background(), "default", "my-app")

	require.NoError(t, err)
	assert.NotNil(t, baseline)
	// Should have made multiple queries (avg CPU, avg memory, peak CPU, peak memory)
	assert.GreaterOrEqual(t, callCount, 1)
}

func TestPrometheusClient_EmptyResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := PrometheusResponse{
			Status: "success",
		}
		response.Data.ResultType = "vector"
		response.Data.Result = []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
			Values [][]interface{}   `json:"values"`
		}{}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	baseline, err := client.GetResourceBaseline(context.Background(), "default", "unknown-app")

	// Should return error when no metrics found
	assert.Error(t, err)
	assert.Nil(t, baseline)
}

func TestPrometheusClient_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal Server Error"))
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	baseline, err := client.GetResourceBaseline(context.Background(), "default", "my-app")

	assert.Error(t, err)
	assert.Nil(t, baseline)
}

func TestPrometheusClient_InvalidURL(t *testing.T) {
	client := NewPrometheusClient("http://invalid-url:9999")
	baseline, err := client.GetResourceBaseline(context.Background(), "default", "my-app")

	assert.Error(t, err)
	assert.Nil(t, baseline)
}

func TestPrometheusClient_EmptyURL(t *testing.T) {
	client := NewPrometheusClient("")
	baseline, err := client.GetResourceBaseline(context.Background(), "default", "my-app")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
	assert.Nil(t, baseline)
}

func TestPrometheusClient_IsAvailable(t *testing.T) {
	// Test with working server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	assert.True(t, client.IsAvailable(context.Background()))

	// Test with unavailable server
	client2 := NewPrometheusClient("http://localhost:99999")
	assert.False(t, client2.IsAvailable(context.Background()))

	// Test with empty URL
	client3 := NewPrometheusClient("")
	assert.False(t, client3.IsAvailable(context.Background()))
}

func TestFormatMemory(t *testing.T) {
	tests := []struct {
		name     string
		bytes    float64
		expected string
	}{
		{
			name:     "megabytes",
			bytes:    128 * 1024 * 1024, // 128Mi
			expected: "128Mi",
		},
		{
			name:     "gigabytes",
			bytes:    2 * 1024 * 1024 * 1024, // 2Gi
			expected: "2.0Gi",
		},
		{
			name:     "small megabytes",
			bytes:    64 * 1024 * 1024, // 64Mi
			expected: "64Mi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatMemory(tt.bytes)
			assert.Equal(t, tt.expected, result)
		})
	}
}
