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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// PrometheusClient queries Prometheus for container metrics.
type PrometheusClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewPrometheusClient creates a new Prometheus client.
func NewPrometheusClient(baseURL string) *PrometheusClient {
	return &PrometheusClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// PrometheusResponse represents the Prometheus API response.
type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`  // [timestamp, value]
			Values [][]any           `json:"values"` // for range queries
		} `json:"result"`
	} `json:"data"`
}

// GetResourceBaseline queries Prometheus for resource usage baseline.
func (c *PrometheusClient) GetResourceBaseline(ctx context.Context, namespace, appName string) (*dorguv1.ResourceBaseline, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("prometheus URL not configured")
	}

	baseline := &dorguv1.ResourceBaseline{}

	// Query average CPU usage over the last hour
	avgCPU, err := c.queryAvgCPU(ctx, namespace, appName)
	if err == nil && avgCPU != "" {
		baseline.AvgCPU = avgCPU
	}

	// Query average memory usage over the last hour
	avgMemory, err := c.queryAvgMemory(ctx, namespace, appName)
	if err == nil && avgMemory != "" {
		baseline.AvgMemory = avgMemory
	}

	// Query peak CPU usage over the last hour
	peakCPU, err := c.queryPeakCPU(ctx, namespace, appName)
	if err == nil && peakCPU != "" {
		baseline.PeakCPU = peakCPU
	}

	// Query peak memory usage over the last hour
	peakMemory, err := c.queryPeakMemory(ctx, namespace, appName)
	if err == nil && peakMemory != "" {
		baseline.PeakMemory = peakMemory
	}

	// Return nil if no metrics were found
	if baseline.AvgCPU == "" && baseline.AvgMemory == "" && baseline.PeakCPU == "" && baseline.PeakMemory == "" {
		return nil, fmt.Errorf("no metrics found for %s/%s", namespace, appName)
	}

	return baseline, nil
}

// queryAvgCPU queries average CPU usage.
func (c *PrometheusClient) queryAvgCPU(ctx context.Context, namespace, appName string) (string, error) {
	// PromQL: avg over last hour of container CPU usage rate
	query := fmt.Sprintf(
		`avg(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s.*",container!="POD",container!=""}[5m])) * 1000`,
		namespace, appName,
	)

	value, err := c.query(ctx, query)
	if err != nil {
		return "", err
	}

	// Convert to millicores
	return fmt.Sprintf("%.0fm", value), nil
}

// queryAvgMemory queries average memory usage.
func (c *PrometheusClient) queryAvgMemory(ctx context.Context, namespace, appName string) (string, error) {
	// PromQL: avg memory usage over last hour
	query := fmt.Sprintf(
		`avg(container_memory_working_set_bytes{namespace="%s",pod=~"%s.*",container!="POD",container!=""})`,
		namespace, appName,
	)

	value, err := c.query(ctx, query)
	if err != nil {
		return "", err
	}

	// Convert to Mi
	return formatMemory(value), nil
}

// queryPeakCPU queries peak CPU usage.
func (c *PrometheusClient) queryPeakCPU(ctx context.Context, namespace, appName string) (string, error) {
	// PromQL: max CPU usage rate over last hour
	query := fmt.Sprintf(
		`max_over_time(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s.*",container!="POD",container!=""}[5m])[1h:]) * 1000`,
		namespace, appName,
	)

	value, err := c.query(ctx, query)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%.0fm", value), nil
}

// queryPeakMemory queries peak memory usage.
func (c *PrometheusClient) queryPeakMemory(ctx context.Context, namespace, appName string) (string, error) {
	// PromQL: max memory usage over last hour
	query := fmt.Sprintf(
		`max_over_time(container_memory_working_set_bytes{namespace="%s",pod=~"%s.*",container!="POD",container!=""}[1h])`,
		namespace, appName,
	)

	value, err := c.query(ctx, query)
	if err != nil {
		return "", err
	}

	return formatMemory(value), nil
}

// query executes a Prometheus instant query and returns the first result value.
func (c *PrometheusClient) query(ctx context.Context, promQL string) (float64, error) {
	endpoint := fmt.Sprintf("%s/api/v1/query", c.baseURL)

	params := url.Values{}
	params.Set("query", promQL)

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return 0, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("prometheus query failed: %s - %s", resp.Status, string(body))
	}

	var promResp PrometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		return 0, err
	}

	if promResp.Status != "success" {
		return 0, fmt.Errorf("prometheus query failed: %s", promResp.Status)
	}

	if len(promResp.Data.Result) == 0 {
		return 0, fmt.Errorf("no results")
	}

	// Extract value from first result
	result := promResp.Data.Result[0]
	if len(result.Value) < 2 {
		return 0, fmt.Errorf("invalid result format")
	}

	valueStr, ok := result.Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("invalid value type")
	}

	return strconv.ParseFloat(valueStr, 64)
}

// IsAvailable checks if Prometheus is reachable.
func (c *PrometheusClient) IsAvailable(ctx context.Context) bool {
	if c.baseURL == "" {
		return false
	}

	endpoint := fmt.Sprintf("%s/api/v1/status/runtimeinfo", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return false
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode == http.StatusOK
}

// formatMemory converts bytes to human-readable format (Mi or Gi).
func formatMemory(bytes float64) string {
	const (
		Mi = 1024 * 1024
		Gi = 1024 * 1024 * 1024
	)

	if bytes >= Gi {
		return fmt.Sprintf("%.1fGi", bytes/Gi)
	}
	return fmt.Sprintf("%.0fMi", bytes/Mi)
}
