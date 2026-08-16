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

package main

import (
	"flag"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// operatorConfig holds all operator configuration parsed from command-line flags.
type operatorConfig struct {
	// Metrics server
	metricsAddr     string
	metricsCertPath string
	metricsCertName string
	metricsCertKey  string
	secureMetrics   bool

	// Health probes
	probeAddr string

	// Webhook
	enableWebhook   bool
	webhookMode     string
	webhookCertPath string
	webhookCertName string
	webhookCertKey  string

	// Leader election
	enableLeaderElection bool

	// HTTP/2
	enableHTTP2 bool

	// ArgoCD integration
	enableArgoCD bool

	// Prometheus integration
	prometheusURL string

	// WebSocket server
	enableWebSocket bool
	webSocketAddr   string

	// Health check reconciler
	enableHealthCheck   bool
	healthCheckInterval time.Duration

	// Metrics-server integration
	enableMetricsServer bool

	// LLM (AI diagnosis)
	llmProvider string // "claude", "gemini", or "" (disabled)
	llmAPIKey   string // API key (overrides env vars)
	llmModel    string // model override

	// AI remediation planning (independent of AI diagnosis).
	enableAIRemediation bool // default: on when an LLM provider+key is configured

	// Bootstrap
	autoCreateClusterPersona     bool
	clusterPersonaEnsureInterval time.Duration

	// Logging
	zapOpts zap.Options
}

// parseFlags parses command-line flags and returns the operator configuration.
func parseFlags() operatorConfig {
	cfg := operatorConfig{}

	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&cfg.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&cfg.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&cfg.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&cfg.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&cfg.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&cfg.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&cfg.metricsCertName, "metrics-cert-name", "tls.crt",
		"The name of the metrics server certificate file.")
	flag.StringVar(&cfg.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&cfg.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(&cfg.enableWebhook, "enable-webhook", false,
		"Enable the validating webhook for Deployment resources against ApplicationPersona constraints.")
	flag.StringVar(&cfg.webhookMode, "webhook-mode", "advisory",
		"Webhook validation mode: 'advisory' (warn only) or 'enforcing' (reject on errors).")
	flag.BoolVar(&cfg.enableArgoCD, "enable-argocd", true,
		"Enable ArgoCD Application watching for sync status integration.")
	flag.StringVar(&cfg.prometheusURL, "prometheus-url", "",
		"Prometheus server URL for metrics baseline learning (e.g., http://prometheus:9090).")
	flag.BoolVar(&cfg.enableWebSocket, "enable-websocket", false,
		"Enable WebSocket server for CLI real-time communication.")
	flag.StringVar(&cfg.webSocketAddr, "websocket-addr", ":9090",
		"Address for the WebSocket server to listen on.")
	flag.BoolVar(&cfg.enableHealthCheck, "enable-health-check", false,
		"Enable health check reconciler for Phase 2a detection and diagnosis.")
	flag.DurationVar(&cfg.healthCheckInterval, "health-check-interval", 60*time.Second,
		"Health check reconciler interval.")
	flag.BoolVar(&cfg.enableMetricsServer, "enable-metrics-server", true,
		"Enable metrics-server integration for detection.")

	// LLM flags
	flag.StringVar(&cfg.llmProvider, "llm-provider", "",
		"LLM provider for AI-enhanced diagnosis: 'claude' or 'gemini' (default: disabled).")
	flag.StringVar(&cfg.llmAPIKey, "llm-api-key", "",
		"API key for the LLM provider (overrides ANTHROPIC_API_KEY / GEMINI_API_KEY env vars).")
	flag.StringVar(&cfg.llmModel, "llm-model", "",
		"Override the default model for the LLM provider.")
	flag.BoolVar(&cfg.enableAIRemediation, "enable-ai-remediation", true,
		"Enable AI-generated ordered remediation plans (requires --llm-provider=claude + key). "+
			"When disabled, the proposer uses the deterministic rule-based path only.")
	flag.BoolVar(&cfg.autoCreateClusterPersona, "auto-create-cluster-persona", true,
		"Auto-create a default ClusterPersona named 'dorgu-cluster' if none exists on startup.")
	flag.DurationVar(&cfg.clusterPersonaEnsureInterval, "cluster-persona-ensure-interval", 2*time.Minute,
		"How often the operator re-ensures the auto-created ClusterPersona exists (clamped to a 30s minimum). "+
			"Acts as a self-healing safety net so the persona converges even if the startup bootstrap missed.")

	cfg.zapOpts = zap.Options{Development: true}
	cfg.zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	return cfg
}
