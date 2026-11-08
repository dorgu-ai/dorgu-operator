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
	"os"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/llm"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation/planner"
)

// healthPipeline is everything the health-check path needs between detection
// and the remediation controller: the one deny-list, the diagnosis engine with
// its providers, the optional AI planner, and the proposer that uses them.
//
// It is built in one place so a test can assert how the pieces are wired. The
// safety checker is the single deny-list; the diagnosis provider's model call
// and the proposer are gated by the same instance, and a second checker built
// without the operator namespace would silently send dorgu-system incidents to
// the model.
type healthPipeline struct {
	SafetyChecker      *remediation.SafetyCheckerImpl
	DiagnosisProviders []diagnosis.DiagnosisProvider
	DiagnosisEngine    *diagnosis.Engine

	// Planner is nil unless AI remediation planning is enabled and configured.
	Planner planner.Planner

	Proposer *remediation.Proposer
}

// buildHealthPipeline assembles the safety checker, diagnosis providers,
// planner and proposer from the operator configuration. operatorNS is the
// operator's own namespace ("" when unknown); the deny-list protects it.
//
// It makes no network calls: LLM clients are constructed but not used.
func buildHealthPipeline(
	c client.Client,
	cfg operatorConfig,
	operatorNS string,
	log logr.Logger,
) healthPipeline {
	// The safety checker is built first because it is the single deny-list: it
	// gates the remediation proposer and the diagnosis provider's model call
	// alike.
	safetyChecker := remediation.NewSafetyChecker(c, log,
		remediation.WithProtectedNamespaces(operatorNS),
		remediation.WithProtectedNamespaces(cfg.safetyExcludedNamespaces...))

	providers := []diagnosis.DiagnosisProvider{diagnosis.NewRuleBasedProvider(log)}

	// aiPlanner, when set, enables AI-generated ordered remediation plans in
	// the proposer. It degrades gracefully to rules.
	var aiPlanner planner.Planner

	if cfg.llmProvider != "" {
		apiKey := resolveLLMAPIKey(cfg)
		if apiKey != "" {
			llmClient, llmErr := llm.NewClient(cfg.llmProvider, apiKey)
			if llmErr != nil {
				log.Error(llmErr, "failed to create LLM client, continuing without AI diagnosis")
			} else {
				if cfg.llmModel != "" {
					if setter, ok := llmClient.(interface{ SetModel(string) }); ok {
						setter.SetModel(cfg.llmModel)
					}
				}
				providers = append(providers, diagnosis.NewAIProvider(llmClient, safetyChecker, log))
				log.Info("AI diagnosis enabled", "provider", cfg.llmProvider)
			}

			// AI remediation planning is Anthropic-only for v1 and toggled
			// independently of AI diagnosis.
			if cfg.enableAIRemediation && cfg.llmProvider == "claude" {
				claudePlanner, plannerErr := planner.NewClaudePlanner(apiKey)
				if plannerErr != nil {
					log.Error(plannerErr, "failed to create AI remediation planner, continuing with rule-based remediation")
				} else {
					claudePlanner.SetModel(cfg.llmModel)
					aiPlanner = claudePlanner
					log.Info("AI remediation planning enabled", "provider", cfg.llmProvider)
				}
			}
		} else {
			log.Info("LLM provider configured but no API key found, AI diagnosis disabled",
				"provider", cfg.llmProvider)
		}
	}

	var proposerOpts []remediation.ProposerOption
	if aiPlanner != nil {
		proposerOpts = append(proposerOpts, remediation.WithPlanner(aiPlanner))
	}

	return healthPipeline{
		SafetyChecker:      safetyChecker,
		DiagnosisProviders: providers,
		DiagnosisEngine:    diagnosis.NewEngine(log, providers...),
		Planner:            aiPlanner,
		Proposer:           remediation.NewProposer(c, safetyChecker, log, proposerOpts...),
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
