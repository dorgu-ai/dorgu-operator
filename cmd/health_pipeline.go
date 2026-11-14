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
	"math"
	"os"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// healthPipeline is everything the health-check path needs between detection
// and the remediation controller: the one gate (deny-list included), the
// diagnosis engine with its providers, the optional AI planner, and the
// proposer that uses them.
//
// It is built in one place so a test can assert how the pieces are wired. There
// is exactly one gate: the diagnosis provider's model call and the proposer are
// gated by the same instance, and a second gate built without the operator
// namespace would silently send dorgu-system incidents to the model.
type healthPipeline struct {
	Gate               *gate.Gate
	DiagnosisProviders []diagnosis.DiagnosisProvider
	DiagnosisEngine    *diagnosis.Engine

	// Planner is nil unless AI remediation planning is enabled and configured.
	Planner planner.Planner

	// Intake decides every RemediationProposal. The proposer and the proposal
	// reconciler share it, so they share one rate limit and one set of persona locks.
	Intake *intake.Processor

	Proposer *remediation.Proposer
}

// buildHealthPipeline assembles the gate, diagnosis providers,
// planner and proposer from the operator configuration. reader is the uncached
// API reader the gate decides on where a stale cache would be permissive; nil
// falls back to the client. operatorNS is the
// operator's own namespace ("" when unknown); the deny-list protects it.
//
// It makes no network calls: LLM clients are constructed but not used.
func buildHealthPipeline(
	c client.Client,
	reader client.Reader,
	cfg operatorConfig,
	operatorNS string,
	log logr.Logger,
) healthPipeline {
	// The gate is built first because it is the single deny-list and the single
	// decision point: it gates the remediation proposer and the diagnosis
	// provider's model call alike.
	gateOpts := []gate.Option{
		gate.WithProtectedNamespaces(operatorNS),
		gate.WithProtectedNamespaces(cfg.safetyExcludedNamespaces...),
	}
	if operatorNS == "" {
		// The deny-list cannot protect a namespace it cannot name. Fail closed: the
		// gate refuses every proposal, rule-based ones included, rather than risk
		// planning a change in the namespace Dorgu itself runs in.
		gateOpts = append(gateOpts, gate.WithOperatorNamespaceUnknown())
	}
	if reader != nil {
		// Sibling RemediationActions (rate limit, concurrency, dedup) and the live
		// workload at approval are read from the API server, not the cache, so a
		// proposal made a moment ago is visible to the next decision (R11).
		gateOpts = append(gateOpts, gate.WithReader(reader))
	}
	g := gate.New(c, log, gateOpts...)

	providers := []diagnosis.DiagnosisProvider{diagnosis.NewRuleBasedProvider(log)}

	// aiPlanner, when set, enables AI-generated ordered remediation plans in
	// the proposer. It degrades gracefully to rules.
	var aiPlanner planner.Planner

	if reason := aiDisabledReason(cfg.llmProvider, operatorNS); reason != "" {
		// Fail closed. Detection, rule-based diagnosis and rule-based proposals
		// carry on; nothing goes to a model.
		log.Error(nil, "AI diagnosis and AI remediation planning are DISABLED: "+reason,
			"provider", cfg.llmProvider)
	} else if cfg.llmProvider != "" {
		apiKey := resolveLLMAPIKey(cfg)
		if apiKey != "" {
			// One client serves diagnosis and planning, so there is one place
			// that sends a request and one place that counts it.
			llmClient, llmErr := llm.NewClient(cfg.llmProvider, apiKey, llm.WithModel(cfg.llmModel))
			if llmErr != nil {
				log.Error(llmErr, "failed to create LLM client, continuing without AI diagnosis")
			} else {
				providers = append(providers, diagnosis.NewAIProvider(llmClient, g, log,
					diagnosis.WithCacheTTL(cfg.llmCacheTTL)))
				log.Info("AI diagnosis enabled", "provider", cfg.llmProvider, "cacheTTL", cfg.llmCacheTTL)

				// AI remediation planning is Anthropic-only for v1 and toggled
				// independently of AI diagnosis.
				if cfg.enableAIRemediation && cfg.llmProvider == "claude" {
					llmPlanner, plannerErr := planner.New(llmClient)
					if plannerErr != nil {
						log.Error(plannerErr, "failed to create AI remediation planner, continuing with rule-based remediation")
					} else {
						aiPlanner = planner.NewCachedPlanner(llmPlanner, cfg.llmCacheTTL, log)
						log.Info("AI remediation planning enabled", "provider", cfg.llmProvider)
					}
				}
			}
		} else {
			log.Info("LLM provider configured but no API key found, AI diagnosis disabled",
				"provider", cfg.llmProvider)
		}
	}

	// One intake for everyone: Dorgu's planner proposes through it as the operator
	// ServiceAccount, and the proposal reconciler decides everyone else's.
	proposalIntake := intake.New(intake.Config{
		Client:           c,
		Reader:           reader,
		Gate:             g,
		Incidents:        &controller.ProposalIncidents{Client: c},
		OperatorUsername: intake.OperatorUsername(operatorNS),
		Logger:           log,
	})

	proposerOpts := []remediation.ProposerOption{remediation.WithIntake(proposalIntake)}
	if aiPlanner != nil {
		proposerOpts = append(proposerOpts, remediation.WithPlanner(aiPlanner))
	}

	return healthPipeline{
		Gate:               g,
		DiagnosisProviders: providers,
		DiagnosisEngine:    diagnosis.NewEngine(log, providers...),
		Planner:            aiPlanner,
		Intake:             proposalIntake,
		Proposer:           remediation.NewProposer(c, g, log, proposerOpts...),
	}
}

// resolveLLMAPIKey returns the configured key, falling back to the provider's
// conventional environment variable.
func resolveLLMAPIKey(cfg operatorConfig) string {
	if cfg.llmAPIKey != "" {
		return cfg.llmAPIKey
	}
	switch cfg.llmProvider {
	case "claude":
		return os.Getenv("ANTHROPIC_API_KEY")
	case "gemini":
		return os.Getenv("GEMINI_API_KEY")
	}
	return ""
}

// newPodCollector builds the pod collector with the gate's deny-list, so
// detection and the gate skip the same namespaces: kube-system, the operator's
// own, the installer's, and every ClusterPersona excludeNamespaces entry.
func newPodCollector(
	c client.Reader, g *gate.Gate, log logr.Logger, opts ...detection.PodCollectorOption,
) *detection.PodCollector {
	all := append([]detection.PodCollectorOption{detection.WithNamespaceFilter(g)}, opts...)
	return detection.NewPodCollector(c, log, all...)
}

// terminatedStaleAfter is the grace before a container's last termination counts
// as history: the detector's default, or twice the health-check interval when
// that is longer, so a kill is always seen by at least one cycle first.
func terminatedStaleAfter(cfg operatorConfig) time.Duration {
	return max(detection.DefaultTerminatedStaleAfter, 2*cfg.healthCheckInterval)
}

// podCollectorOptions turns the probe-detection flags into collector options.
//
// events must be an uncached reader: kubelet probe events are read only for the
// few pods that already carry a signal, which does not justify an informer on
// every Event in the cluster.
func podCollectorOptions(cfg operatorConfig, events client.Reader) []detection.PodCollectorOption {
	return []detection.PodCollectorOption{
		detection.WithReadinessThreshold(cfg.readinessThreshold),
		detection.WithLivenessRestarts(clampInt32(cfg.livenessRestartCount), cfg.livenessRestartWindow),
		detection.WithTerminatedStaleAfter(terminatedStaleAfter(cfg)),
		detection.WithEventReader(events),
	}
}

// clampInt32 narrows a flag value to int32, saturating instead of wrapping.
func clampInt32(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	default:
		return int32(n)
	}
}
