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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// PrometheusThrottleQuery is the true throttling signal: the share of CPU
	// scheduling periods in which the container was throttled, over five minutes.
	PrometheusThrottleQuery = "rate(container_cpu_cfs_throttled_periods_total[5m]) / rate(container_cpu_cfs_periods_total[5m])"

	// PrometheusThrottleRatio is the ratio at or above which the signal fires: a
	// container throttled in a quarter of its periods or more.
	PrometheusThrottleRatio = 0.25

	// PrometheusTimeout bounds one query, connection included.
	PrometheusTimeout = 10 * time.Second

	// PrometheusMaxResponseBytes bounds the body read from the endpoint. A cluster
	// of a few thousand containers answers in well under a megabyte.
	PrometheusMaxResponseBytes int64 = 4 << 20

	// PrometheusMaxSeries bounds how many series one answer may carry.
	PrometheusMaxSeries = 20000

	// maxPrometheusURLLength matches the CRD's maxLength.
	maxPrometheusURLLength = 2048
)

// ErrPrometheusNotConfigured says no Prometheus URL is set. It is an absence, not a
// fault: the collector reads metrics.k8s.io instead and says nothing is wrong.
var ErrPrometheusNotConfigured = errors.New("no Prometheus URL is configured")

// ErrPrometheusURLInvalid marks a spec.metrics.prometheus.url the operator refuses
// (scheme, credentials, query string, whitespace...). It is the admin's typo, so the
// status says so instead of calling the endpoint unreachable.
var ErrPrometheusURLInvalid = errors.New("the Prometheus URL is not valid")

// urlError is a URL validation failure: its text is the specific reason, and it is
// an ErrPrometheusURLInvalid to errors.Is.
type urlError string

func (e urlError) Error() string      { return string(e) }
func (urlError) Is(target error) bool { return target == ErrPrometheusURLInvalid }

func invalidURL(msg string) error { return urlError(msg) }

// ThrottleSample is one container's throttled-period ratio, already checked.
type ThrottleSample struct {
	Namespace string
	Pod       string
	Container string
	// Ratio is throttled periods over periods, in [0, 1].
	Ratio float64
}

// ThrottleSource is the optional true-throttling source.
type ThrottleSource interface {
	// Query returns the current ratio per container. ErrPrometheusNotConfigured
	// means there is no source; any other error means there is one and it failed.
	Query(ctx context.Context) ([]ThrottleSample, error)
}

// PrometheusThrottleSource queries a Prometheus-compatible API. Its answer is
// untrusted: it comes from a URL an admin typed, from a server Dorgu does not run.
// So it is bounded in time and size, never follows a redirect, and every label and
// value is checked before it can become a sample.
type PrometheusThrottleSource struct {
	url       func(ctx context.Context) (string, error)
	http      *http.Client
	maxBytes  int64
	maxSeries int
}

// PrometheusOption configures a PrometheusThrottleSource.
type PrometheusOption func(*PrometheusThrottleSource)

// WithPrometheusTimeout overrides PrometheusTimeout.
func WithPrometheusTimeout(d time.Duration) PrometheusOption {
	return func(s *PrometheusThrottleSource) {
		if d > 0 {
			s.http.Timeout = d
		}
	}
}

// WithPrometheusMaxBytes overrides PrometheusMaxResponseBytes.
func WithPrometheusMaxBytes(n int64) PrometheusOption {
	return func(s *PrometheusThrottleSource) {
		if n > 0 {
			s.maxBytes = n
		}
	}
}

// NewPrometheusThrottleSource builds a source whose base URL is read from urlFn on
// every query, so a change to the ClusterPersona takes effect without a restart.
func NewPrometheusThrottleSource(urlFn func(ctx context.Context) (string, error), opts ...PrometheusOption) *PrometheusThrottleSource {
	s := &PrometheusThrottleSource{
		url: urlFn,
		http: &http.Client{
			Timeout: PrometheusTimeout,
			// A redirect is how a metrics URL becomes a request to somewhere the admin
			// never named (a cloud metadata address, another service). Refuse them all.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		},
		maxBytes:  PrometheusMaxResponseBytes,
		maxSeries: PrometheusMaxSeries,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ValidatePrometheusURL checks a Prometheus base URL and returns it normalised. Only
// http and https are accepted, with a host, and with no credentials, query string or
// fragment: the operator adds the path and the query itself.
func ValidatePrometheusURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", invalidURL("prometheus url is empty")
	}
	if len(raw) > maxPrometheusURLLength {
		return "", invalidURL(fmt.Sprintf("prometheus url is longer than %d characters", maxPrometheusURLLength))
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", invalidURL("prometheus url contains whitespace or a control character")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", invalidURL("prometheus url cannot be parsed")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", invalidURL(fmt.Sprintf("prometheus url scheme must be http or https, not %q", u.Scheme))
	}
	switch {
	case u.Hostname() == "":
		return "", invalidURL("prometheus url has no host")
	case u.User != nil:
		return "", invalidURL("prometheus url must not carry credentials")
	case u.RawQuery != "" || strings.Contains(raw, "?"):
		return "", invalidURL("prometheus url must not carry a query string")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "", invalidURL("prometheus url must not carry a fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	return strings.TrimRight(u.String(), "/"), nil
}

// Query implements ThrottleSource.
func (s *PrometheusThrottleSource) Query(ctx context.Context) ([]ThrottleSample, error) {
	raw, err := s.url(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the Prometheus URL: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, ErrPrometheusNotConfigured
	}
	base, err := ValidatePrometheusURL(raw)
	if err != nil {
		return nil, err
	}

	endpoint := base + "/api/v1/query?" + url.Values{"query": {PrometheusThrottleQuery}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("could not build the Prometheus request")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		// url.Error carries the full URL; it is the admin's own, but keep it out of
		// anything that might be shown further than a log.
		return nil, fmt.Errorf("prometheus request failed: %w", unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The body is never read into an error: it is remote text.
		return nil, fmt.Errorf("prometheus answered HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the Prometheus response: %w", unwrapURLError(err))
	}
	if int64(len(body)) > s.maxBytes {
		return nil, fmt.Errorf("prometheus response is over the %d byte limit", s.maxBytes)
	}
	return s.parse(body)
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

type promEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []json.RawMessage `json:"value"`
}

// parse turns a response body into samples. A document that is not a successful
// instant vector is an error; a series inside one that is not what it should be is
// dropped, so one hostile series cannot hide the others or invent a signal.
func (s *PrometheusThrottleSource) parse(body []byte) ([]ThrottleSample, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var env promEnvelope
	if err := dec.Decode(&env); err != nil {
		return nil, errors.New("prometheus response is not valid JSON")
	}
	if dec.More() {
		return nil, errors.New("prometheus response has content after the JSON document")
	}
	if env.Status != "success" {
		return nil, fmt.Errorf("prometheus reported status %q", sanitizeForError(env.Status))
	}
	if env.Data.ResultType != "vector" {
		return nil, errors.New("prometheus result is not an instant vector")
	}
	if len(env.Data.Result) > s.maxSeries {
		return nil, fmt.Errorf("prometheus returned %d series, over the limit of %d", len(env.Data.Result), s.maxSeries)
	}

	samples := make([]ThrottleSample, 0, len(env.Data.Result))
	for _, raw := range env.Data.Result {
		if sample, ok := sampleFrom(raw); ok {
			samples = append(samples, sample)
		}
	}
	return samples, nil
}

// sanitizeForError keeps a short alphanumeric token from a remote field.
func sanitizeForError(in string) string {
	if len(in) > 16 {
		return "unexpected"
	}
	for _, r := range in {
		if !unicode.IsLetter(r) {
			return "unexpected"
		}
	}
	return in
}

// sampleFrom checks one series. Namespace, pod and container must be valid
// Kubernetes names, and the value a finite number in [0, 1]; anything else is dropped.
func sampleFrom(raw json.RawMessage) (ThrottleSample, bool) {
	var series promSeries
	if err := json.Unmarshal(raw, &series); err != nil || len(series.Value) != 2 {
		return ThrottleSample{}, false
	}
	ns, pod, container := series.Metric["namespace"], series.Metric["pod"], series.Metric["container"]
	if len(validation.IsDNS1123Label(ns)) > 0 ||
		len(validation.IsDNS1123Subdomain(pod)) > 0 ||
		len(validation.IsDNS1123Label(container)) > 0 {
		return ThrottleSample{}, false
	}

	// The sample value is a JSON string ("0.31"), which also carries NaN and +Inf.
	var text string
	if err := json.Unmarshal(series.Value[1], &text); err != nil {
		return ThrottleSample{}, false
	}
	ratio, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
		return ThrottleSample{}, false
	}
	return ThrottleSample{Namespace: ns, Pod: pod, Container: container, Ratio: ratio}, true
}
