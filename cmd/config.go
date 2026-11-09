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
	"fmt"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
)

// DefaultRemediationPendingTTL is how long a proposal may wait for a human
// before it expires.
const DefaultRemediationPendingTTL = 24 * time.Hour

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

	// Leader election
	enableLeaderElection bool

	// HTTP/2
	enableHTTP2 bool

	// ArgoCD integration
	enableArgoCD bool

	// Prometheus integration
	prometheusURL string

	// Health check reconciler
	enableHealthCheck   bool
	healthCheckInterval time.Duration

	// Probe-failure detection (B9). Zero leaves the detector's default in place.
	readinessThreshold    time.Duration
	livenessRestartCount  int
	livenessRestartWindow time.Duration

	// Recurrence cap and retention of Resolved incidents (B9 review F-3).
	recurrenceCap     int
	recurrenceWindow  time.Duration
	resolvedRetention time.Duration

	// Metrics-server integration
	enableMetricsServer bool

	// Safety: namespaces the deny-list refuses on top of kube-system and the
	// operator's own namespace.
	safetyExcludedNamespaces []string

	// Remediation: how long a proposal may wait for approval before it expires.
	// Zero disables the TTL.
	remediationPendingTTL time.Duration

	// LLM (AI diagnosis)
	llmProvider string // "claude", "gemini", or "" (disabled)
	llmAPIKey   string // API key (overrides env vars)
	llmModel    string // model override

	// llmCacheTTL is how long a model answer is reused while a group's signal
	// fingerprint is unchanged. Zero disables the cache (a call every cycle).
	llmCacheTTL time.Duration

	// AI remediation planning (independent of AI diagnosis).
	enableAIRemediation bool // default: on when an LLM provider+key is configured

	// Event pipeline: watches Kubernetes Events and writes DorguEvent records.
	// Off by default; nothing reads what it produces yet.
	enableEventPipeline bool

	// DorguEvent record retention (only used when the event pipeline is on)
	dorguEventRetention  time.Duration
	dorguEventMaxRecords int

	// Bootstrap
	autoCreateClusterPersona     bool
	clusterPersonaEnsureInterval time.Duration

	// Logging
	zapOpts zap.Options
}

// parseFlags parses the process's command-line flags. A bad flag exits, as the
// standard flag package does.
func parseFlags() operatorConfig {
	cfg, err := parseFlagSet(flag.CommandLine, os.Args[1:])
	if err != nil {
		// flag.CommandLine is ExitOnError, so this is only reached by a caller that
		// changed that; fail the same way.
		fmt.Fprintln(os.Stderr, "invalid flags:", err)
		os.Exit(2)
	}
	return cfg
}

// parseFlagSet binds every operator flag on fs and parses args. It exists so a
// test can parse a fresh FlagSet; the process uses parseFlags.
func parseFlagSet(fs *flag.FlagSet, args []string) (operatorConfig, error) {
	cfg := operatorConfig{}

	fs.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	fs.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&cfg.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	fs.BoolVar(&cfg.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	fs.StringVar(&cfg.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	fs.StringVar(&cfg.metricsCertName, "metrics-cert-name", "tls.crt",
		"The name of the metrics server certificate file.")
	fs.StringVar(&cfg.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	fs.BoolVar(&cfg.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics server")
	fs.BoolVar(&cfg.enableArgoCD, "enable-argocd", true,
		"Enable ArgoCD Application watching for sync status integration.")
	fs.StringVar(&cfg.prometheusURL, "prometheus-url", "",
		"Prometheus server URL for metrics baseline learning (e.g., http://prometheus:9090).")
	fs.BoolVar(&cfg.enableHealthCheck, "enable-health-check", true,
		"Run detection, diagnosis and remediation proposals. When false, Dorgu records persona health only "+
			"and never opens an incident or proposes a fix.")
	fs.DurationVar(&cfg.healthCheckInterval, "health-check-interval", 60*time.Second,
		"Health check reconciler interval.")
	fs.DurationVar(&cfg.readinessThreshold, "readiness-threshold", detection.DefaultReadinessThreshold,
		"How long a running container may stay not Ready before it is reported as a health signal. An app that "+
			"legitimately takes longer to become Ready should have a startupProbe (a container whose startup probe "+
			"has not passed is never reported) or this raised.")
	fs.IntVar(&cfg.livenessRestartCount, "liveness-restart-count", int(detection.DefaultLivenessRestarts),
		"Container restarts within --liveness-restart-window (last exit an Error or SIGKILL) that count as a "+
			"liveness-probe failure signal, without waiting for CrashLoopBackOff.")
	fs.DurationVar(&cfg.livenessRestartWindow, "liveness-restart-window", detection.DefaultLivenessWindow,
		"The window in which --liveness-restart-count restarts make a signal.")
	fs.IntVar(&cfg.recurrenceCap, "incident-recurrence-cap", controller.DefaultRecurrenceCap,
		"How many recurrences of one fault may open a new IncidentMemory within --incident-recurrence-window. "+
			"After that the latest incident is reopened instead (and one Event says so), so a flapping fault cannot "+
			"create an incident per cycle. 0 disables the cap.")
	fs.DurationVar(&cfg.recurrenceWindow, "incident-recurrence-window", controller.DefaultRecurrenceWindow,
		"The window --incident-recurrence-cap counts recurrences in.")
	fs.DurationVar(&cfg.resolvedRetention, "resolved-incident-retention", controller.DefaultResolvedRetention,
		"How long a Resolved IncidentMemory is kept before it is deleted. The newest Resolved incident of every "+
			"fault, and any incident a non-terminal RemediationAction names, is always kept. 0 keeps them forever.")
	fs.BoolVar(&cfg.enableMetricsServer, "enable-metrics-server", true,
		"Enable metrics-server integration for detection.")

	fs.BoolVar(&cfg.enableEventPipeline, "enable-event-pipeline", false,
		"Run the event pipeline: watch Kubernetes Events, classify and correlate them, and write DorguEvent "+
			"records (pruned by the retention flags below). Nothing in Dorgu reads those records yet, so it is off "+
			"by default.")

	// DorguEvent retention. Both bounds are enforced by the same cleaner: a
	// record is removed once it is older than the retention, and the oldest are
	// removed early if the count is over the cap. The cap is what holds on a
	// large cluster, where the arrival rate alone outgrows any time window.
	fs.DurationVar(&cfg.dorguEventRetention, "dorgu-event-retention", events.DefaultTTL,
		"How long a DorguEvent record is kept before the cleaner removes it.")
	fs.IntVar(&cfg.dorguEventMaxRecords, "dorgu-event-max-records", events.DefaultMaxRecords,
		"Maximum DorguEvent records to keep regardless of age; the oldest are removed first. "+
			"Set to 0 to drop the cap and rely on --dorgu-event-retention alone.")

	fs.DurationVar(&cfg.remediationPendingTTL, "remediation-pending-ttl", DefaultRemediationPendingTTL,
		"How long a RemediationAction may wait for approval, counted from its creation, before it becomes "+
			"Expired. Expired is terminal, applies nothing and starts no cooldown. 0 disables the TTL; "+
			"a spec.approval.deadline set by the proposal is still honoured, and can only shorten the window.")

	// LLM flags
	fs.StringVar(&cfg.llmProvider, "llm-provider", "",
		"LLM provider for AI-enhanced diagnosis: 'claude' or 'gemini' (default: disabled).")
	fs.StringVar(&cfg.llmAPIKey, "llm-api-key", "",
		"API key for the LLM provider (overrides ANTHROPIC_API_KEY / GEMINI_API_KEY env vars).")
	fs.StringVar(&cfg.llmModel, "llm-model", "",
		"Override the default model for the LLM provider.")
	fs.DurationVar(&cfg.llmCacheTTL, "llm-cache-ttl", llm.DefaultCacheTTL,
		"How long a diagnosis or plan from the model is reused while the group's signal fingerprint is "+
			"unchanged. A change in the signals asks again at once; this only bounds how stale an unchanged "+
			"answer may get. 0 disables the cache and asks the model every health-check cycle.")
	fs.BoolVar(&cfg.enableAIRemediation, "enable-ai-remediation", true,
		"Enable AI-generated ordered remediation plans (requires --llm-provider=claude + key). "+
			"When disabled, the proposer uses the deterministic rule-based path only.")
	fs.BoolVar(&cfg.autoCreateClusterPersona, "auto-create-cluster-persona", true,
		"Auto-create a default ClusterPersona named 'dorgu-cluster' if none exists on startup.")
	fs.DurationVar(&cfg.clusterPersonaEnsureInterval, "cluster-persona-ensure-interval", 2*time.Minute,
		"How often the operator re-ensures the auto-created ClusterPersona exists (clamped to a 30s minimum). "+
			"Acts as a self-healing safety net so the persona converges even if the startup bootstrap missed.")

	var excludedNamespaces string
	fs.StringVar(&excludedNamespaces, "safety-excluded-namespaces", "",
		"Comma-separated namespaces Dorgu never remediates, in addition to kube-system and the "+
			"operator's own namespace (from POD_NAMESPACE).")

	cfg.zapOpts = zap.Options{Development: true}
	cfg.zapOpts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return operatorConfig{}, err
	}

	cfg.safetyExcludedNamespaces = splitNamespaces(excludedNamespaces)
	return cfg, nil
}

// validate rejects configuration the operator must not start with.
func (c operatorConfig) validate() error {
	if c.remediationPendingTTL < 0 {
		return fmt.Errorf("--remediation-pending-ttl must not be negative (use 0 to disable), got %s",
			c.remediationPendingTTL)
	}
	if c.readinessThreshold < 0 || c.livenessRestartCount < 0 || c.livenessRestartWindow < 0 {
		return fmt.Errorf("--readiness-threshold, --liveness-restart-count and --liveness-restart-window must not be "+
			"negative (omit them for the defaults), got %s, %d, %s",
			c.readinessThreshold, c.livenessRestartCount, c.livenessRestartWindow)
	}
	if c.recurrenceCap < 0 || c.recurrenceWindow < 0 || c.resolvedRetention < 0 {
		return fmt.Errorf("--incident-recurrence-cap, --incident-recurrence-window and --resolved-incident-retention "+
			"must not be negative (use 0 to disable the cap or the pruning), got %d, %s, %s",
			c.recurrenceCap, c.recurrenceWindow, c.resolvedRetention)
	}
	if c.resolvedRetention > 0 && c.recurrenceCap > 0 && c.resolvedRetention < c.recurrenceWindow {
		return fmt.Errorf("--resolved-incident-retention=%s is shorter than --incident-recurrence-window=%s: "+
			"pruning would delete the incidents the recurrence cap counts. Use at least %s, or 0 to keep them",
			c.resolvedRetention, c.recurrenceWindow, c.recurrenceWindow)
	}
	if err := c.validateLivenessWindow(); err != nil {
		return err
	}
	if c.llmCacheTTL < 0 {
		return fmt.Errorf("--llm-cache-ttl must not be negative (use 0 to disable), got %s", c.llmCacheTTL)
	}
	return nil
}

// validateLivenessWindow refuses a liveness window the health check cannot sample.
// The restart history is one observation per cycle, so a window shorter than two
// cycles holds at most one and no delta can ever be computed: the signal would be
// silently dead. It is checked only when detection runs, with the flags' own
// defaults standing in for unset values.
func (c operatorConfig) validateLivenessWindow() error {
	if !c.enableHealthCheck {
		return nil
	}
	interval := c.healthCheckInterval
	if interval == 0 {
		interval = controller.DefaultReconcileInterval
	}
	window := c.livenessRestartWindow
	if window == 0 {
		window = detection.DefaultLivenessWindow
	}
	if window < 2*interval {
		return fmt.Errorf("--liveness-restart-window=%s is shorter than two health-check cycles "+
			"(--health-check-interval=%s): the liveness signal could never fire. Use at least %s",
			window, interval, 2*interval)
	}
	return nil
}

// aiDisabledReason says why AI features must stay off despite an LLM provider
// being configured, or "" when they may run. Today there is one reason: without
// the operator's own namespace the deny-list cannot protect it, so an incident in
// the operator's namespace could be sent to the model. It fails closed: AI is off,
// loudly, and everything that needs no model carries on.
func aiDisabledReason(llmProvider, operatorNS string) string {
	if llmProvider == "" || operatorNS != "" {
		return ""
	}
	return "the operator's namespace is unknown, so the deny-list cannot keep its own namespace away from the model; " +
		"set " + gate.OperatorNamespaceEnv + " (the Helm chart does) to turn AI on"
}

// splitNamespaces parses a comma-separated namespace list, dropping blanks.
func splitNamespaces(list string) []string {
	var out []string
	for ns := range strings.SplitSeq(list, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}
