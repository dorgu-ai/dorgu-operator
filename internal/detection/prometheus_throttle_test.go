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

package detection

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Prometheus response is untrusted input: it comes from a URL an admin typed
// into a CRD, over a network, from a server Dorgu does not control. The source
// bounds its time and size, never follows a redirect, and treats every label and
// value as hostile until it has checked it.

func vector(results ...string) string {
	return fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(results, ","))
}

func series(ns, pod, container, value string) string {
	return fmt.Sprintf(`{"metric":{"namespace":%q,"pod":%q,"container":%q},"value":[1731585600.123,%q]}`, ns, pod, container, value)
}

func promServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *PrometheusThrottleSource) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	src := NewPrometheusThrottleSource(func(context.Context) (string, error) { return srv.URL, nil })
	return srv, src
}

func TestPrometheusThrottle_ParsesAVector(t *testing.T) {
	var gotPath, gotQuery string
	_, src := promServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("query")
		_, _ = w.Write([]byte(vector(
			series("shop", "api-1", "app", "0.31"),
			series("shop", "api-2", "app", "0.02"),
		)))
	})
	samples, err := src.Query(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/query", gotPath)
	assert.Equal(t, PrometheusThrottleQuery, gotQuery)
	require.Len(t, samples, 2)
	assert.Equal(t, ThrottleSample{Namespace: "shop", Pod: "api-1", Container: "app", Ratio: 0.31}, samples[0])
}

func TestPrometheusThrottle_KeepsABasePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(vector()))
	}))
	t.Cleanup(srv.Close)
	src := NewPrometheusThrottleSource(func(context.Context) (string, error) { return srv.URL + "/prometheus/", nil })
	_, err := src.Query(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/prometheus/api/v1/query", gotPath)
}

func TestValidatePrometheusURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"http", "http://prometheus-server.monitoring:9090", false},
		{"https", "https://prometheus.example.com", false},
		{"https with a path prefix", "https://example.com/prometheus", false},
		{"upper-case scheme is still http", "HTTP://prometheus:9090", false},
		{"empty", "", true},
		{"file scheme", "file:///etc/passwd", true},
		{"ftp scheme", "ftp://prometheus:21", true},
		{"gopher scheme", "gopher://prometheus:70/", true},
		{"javascript scheme", "javascript:alert(1)", true},
		{"no scheme", "prometheus:9090", true},
		{"scheme-relative", "//prometheus:9090", true},
		{"no host", "http://", true},
		{"credentials in the URL", "http://user:pass@prometheus:9090", true},
		{"a query string", "http://prometheus:9090/?query=up", true},
		{"a fragment", "http://prometheus:9090/#x", true},
		{"whitespace", "http://prometheus :9090", true},
		{"a control character", "http://prometheus:9090/\x00", true},
		{"newline injection", "http://prometheus:9090/\r\nHost: evil", true},
		{"too long", "http://" + strings.Repeat("a", 3000), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidatePrometheusURL(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, got)
		})
	}
}

func TestPrometheusThrottle_RefusesABadURLWithoutDialling(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)

	for _, raw := range []string{"ftp://" + strings.TrimPrefix(srv.URL, "http://"), "file:///etc/passwd", "http://u:p@" + strings.TrimPrefix(srv.URL, "http://")} {
		src := NewPrometheusThrottleSource(func(context.Context) (string, error) { return raw, nil })
		_, err := src.Query(context.Background())
		require.Error(t, err, raw)
		assert.False(t, errors.Is(err, ErrPrometheusNotConfigured), "a bad URL is a fault, not an absence: %s", raw)
	}
	assert.Zero(t, hits.Load())
}

func TestPrometheusThrottle_NoURLIsNotConfigured(t *testing.T) {
	src := NewPrometheusThrottleSource(func(context.Context) (string, error) { return "", nil })
	_, err := src.Query(context.Background())
	require.ErrorIs(t, err, ErrPrometheusNotConfigured)

	src = NewPrometheusThrottleSource(func(context.Context) (string, error) { return "  ", nil })
	_, err = src.Query(context.Background())
	require.ErrorIs(t, err, ErrPrometheusNotConfigured)
}

func TestPrometheusThrottle_URLProviderErrorIsAFault(t *testing.T) {
	src := NewPrometheusThrottleSource(func(context.Context) (string, error) { return "", errors.New("cannot list ClusterPersonas") })
	_, err := src.Query(context.Background())
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrPrometheusNotConfigured))
}

func TestPrometheusThrottle_TimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	src := NewPrometheusThrottleSource(func(context.Context) (string, error) { return srv.URL, nil },
		WithPrometheusTimeout(100*time.Millisecond))

	start := time.Now()
	_, err := src.Query(context.Background())
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestPrometheusThrottle_DefaultTimeoutIsBounded(t *testing.T) {
	assert.LessOrEqual(t, PrometheusTimeout, 15*time.Second)
	assert.Greater(t, PrometheusTimeout, time.Duration(0))
	assert.LessOrEqual(t, PrometheusMaxResponseBytes, int64(8<<20))
}

func TestPrometheusThrottle_BoundsTheResponse(t *testing.T) {
	// A well-formed document that is simply too big, and a body that never ends.
	big := vector(series("shop", "api-1", "app", "0.5"))
	big = big[:len(big)-3] + strings.Repeat(" ", 4096) + big[len(big)-3:]

	tests := []struct {
		name string
		body func(w http.ResponseWriter)
	}{
		{"padded past the limit", func(w http.ResponseWriter) { _, _ = w.Write([]byte(big)) }},
		{"endless", func(w http.ResponseWriter) {
			chunk := []byte(strings.Repeat("x", 1024))
			for range 1 << 14 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tt.body(w) }))
			t.Cleanup(srv.Close)
			src := NewPrometheusThrottleSource(func(context.Context) (string, error) { return srv.URL, nil },
				WithPrometheusMaxBytes(2048))
			samples, err := src.Query(context.Background())
			require.Error(t, err)
			assert.Empty(t, samples, "a truncated document is never half-believed")
			assert.Contains(t, err.Error(), "limit")
		})
	}
}

func TestPrometheusThrottle_DoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(vector()))
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	src := NewPrometheusThrottleSource(func(context.Context) (string, error) { return redirector.URL, nil })
	_, err := src.Query(context.Background())
	require.Error(t, err)
	assert.Zero(t, targetHits.Load(), "a redirect is how a metrics URL becomes a request to somewhere else")
}

func TestPrometheusThrottle_BadResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"server error", 500, "boom"},
		{"unauthorized", 401, "no"},
		{"not json", 200, "<html>login</html>"},
		{"empty body", 200, ""},
		{"status error", 200, `{"status":"error","errorType":"bad_data","error":"parse error"}`},
		{"matrix, not a vector", 200, `{"status":"success","data":{"resultType":"matrix","result":[]}}`},
		{"scalar", 200, `{"status":"success","data":{"resultType":"scalar","result":[1,"1"]}}`},
		{"trailing garbage", 200, vector() + "{}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, src := promServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			samples, err := src.Query(context.Background())
			require.Error(t, err)
			assert.Empty(t, samples)
			assert.NotContains(t, err.Error(), "login", "the remote body is not echoed into errors, logs or status")
			assert.NotContains(t, err.Error(), "parse error")
		})
	}
}

func TestPrometheusThrottle_DropsHostileSeries(t *testing.T) {
	long := strings.Repeat("a", 300)
	tests := []struct {
		name string
		s    string
	}{
		{"value is not a number", series("shop", "api-1", "app", "lots")},
		{"NaN", series("shop", "api-1", "app", "NaN")},
		{"+Inf", series("shop", "api-1", "app", "+Inf")},
		{"-Inf", series("shop", "api-1", "app", "-Inf")},
		{"negative", series("shop", "api-1", "app", "-0.5")},
		{"above one", series("shop", "api-1", "app", "1.5")},
		{"empty pod", series("shop", "", "app", "0.5")},
		{"empty container", series("shop", "api-1", "", "0.5")},
		{"empty namespace", series("", "api-1", "app", "0.5")},
		{"upper-case namespace", series("Shop", "api-1", "app", "0.5")},
		{"pod name with a newline", series("shop", "api-1\nHost: x", "app", "0.5")},
		{"pod name with a slash", series("shop", "../api", "app", "0.5")},
		{"over-long pod", series("shop", long, "app", "0.5")},
		{"control characters in the container", series("shop", "api-1", "ap\x01p", "0.5")},
		{"missing labels", `{"metric":{},"value":[1,"0.5"]}`},
		{"value is the wrong shape", `{"metric":{"namespace":"shop","pod":"api-1","container":"app"},"value":"0.5"}`},
		{"value has one element", `{"metric":{"namespace":"shop","pod":"api-1","container":"app"},"value":[1]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, src := promServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(vector(tt.s, series("shop", "ok-1", "app", "0.6"))))
			})
			samples, err := src.Query(context.Background())
			require.NoError(t, err)
			require.Len(t, samples, 1, "only the well-formed series survives")
			assert.Equal(t, "ok-1", samples[0].Pod)
		})
	}
}

func TestPrometheusThrottle_CapsTheSeriesCount(t *testing.T) {
	_, src := promServer(t, func(w http.ResponseWriter, _ *http.Request) {
		parts := make([]string, 0, 50)
		for i := range 50 {
			parts = append(parts, series("shop", fmt.Sprintf("api-%d", i), "app", "0.5"))
		}
		_, _ = w.Write([]byte(vector(parts...)))
	})
	src.maxSeries = 10
	samples, err := src.Query(context.Background())
	require.Error(t, err)
	assert.Empty(t, samples)
}

func TestPrometheusThrottle_NeverSendsCredentialsOrHeadersFromTheURL(t *testing.T) {
	var auth string
	_, src := promServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(vector()))
	})
	_, err := src.Query(context.Background())
	require.NoError(t, err)
	assert.Empty(t, auth)
}
