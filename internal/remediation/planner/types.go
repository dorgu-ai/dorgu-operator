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

// Package planner produces AI-generated, ordered remediation plans from rich
// cluster/app/incident context. It is the WS1 surface of the AI-remediation
// sprint: generation + validation only — the proposer maps a RemediationPlan
// into RemediationAction.Steps[] and the executor (unchanged this sprint)
// continues to apply the single back-compat Action.
//
// The package depends only on the typed CRDs, the diagnosis result, and
// detection signals — never on the remediation or controller packages — so it
// can be imported by the proposer without an import cycle.
package planner

import (
	"context"
	"encoding/json"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
)

// RemediationContext is the full context handed to the planner. It is assembled
// by BuildContext from the diagnosed incident plus the surrounding cluster/app
// state and the outcomes of past remediations for the same application.
type RemediationContext struct {
	// Diagnosis is the root-cause analysis that triggered remediation.
	Diagnosis diagnosis.Diagnosis

	// Signals are the raw detection signals behind the diagnosis.
	Signals []detection.Signal

	// AppPersona is the affected application's persona (spec + status, including
	// Learned.ResourceBaseline). Nil when it could not be loaded.
	//
	// The persona is a point-in-time import and drifts from the running
	// workload, so it describes intent, not current reality. Anything the
	// planner states as a present-tense fact must come from Workload instead.
	AppPersona *dorguv1.ApplicationPersona

	// Workload is the live workload read at proposal time: the ground truth for
	// every stated number, every cap, and who owns the Deployment. Nil when no
	// Deployment could be resolved, which the prompt reports honestly rather
	// than papering over with persona values.
	Workload *WorkloadContext

	// ClusterPersona is the singleton cluster persona (environment, policies,
	// selfHealing mode/trustLevel, quotas). Nil when none exists.
	ClusterPersona *dorguv1.ClusterPersona

	// PastIncidents are recent IncidentMemory records for this application,
	// most-recent first and capped (see MaxPastIncidents).
	PastIncidents []dorguv1.IncidentMemory

	// PastRemediations are the RemediationActions referenced by PastIncidents,
	// carried WITH their status (phase + verification result) so the planner can
	// prefer fixes that previously succeeded and avoid ones that failed.
	PastRemediations []dorguv1.RemediationAction
}

// WorkloadContext is the live workload the incident concerns, as read from the
// cluster rather than from the persona.
//
// It exists because run #2 of the clean room caught Dorgu narrating a 96Mi
// memory limit for a pod whose limit was 32Mi, and computing a "2x" increase
// off that stale number. Every present-tense claim the planner makes has to be
// traceable to this struct.
type WorkloadContext struct {
	// Ref is the observed workload record: name, namespace, container, owner,
	// live resources and live image.
	Ref *dorguv1.WorkloadRef

	// Replicas is the Deployment's desired replica count.
	Replicas int32

	// ReadyReplicas is how many replicas are currently ready.
	ReadyReplicas int32

	// PriorImages are image references Dorgu has actually read for this
	// application (the persona's imported-image annotation and its recorded
	// deployment history). They are the only prior versions the planner may
	// name, which is what stops it inventing a "latest stable release" from
	// training data.
	PriorImages []string
}

// RemediationPlan is the structured output the planner returns: a root-cause
// explanation, a confidence score, and an ordered list of steps.
type RemediationPlan struct {
	// RootCause is the planner's concise root-cause explanation.
	RootCause string `json:"rootCause"`

	// Confidence is the planner's confidence in the plan (0.0-1.0).
	Confidence float64 `json:"confidence"`

	// Steps is the ordered remediation plan.
	Steps []PlannedStep `json:"steps"`
}

// PlannedStep is a single ordered action proposed by the planner.
//
// Only steps of type "persona-update" carry a Patch and may ultimately become
// auto-executable; every other type is advisory (recorded for a
// human/CLI/platform to apply), preserving the operator's invariant that it
// never writes workloads.
type PlannedStep struct {
	// Order is the 1-based execution order within the plan.
	Order int32 `json:"order"`

	// Type classifies the step: persona-update|workload-apply|restart|scale|
	// config-change|manual.
	Type string `json:"type"`

	// Description is a human-readable summary of the action.
	Description string `json:"description"`

	// Rationale explains why the step is proposed (AI reasoning).
	Rationale string `json:"rationale"`

	// Risk is the assessed risk level: low|medium|high.
	Risk string `json:"risk"`

	// Patch is the JSON merge patch to apply to the persona spec, present only
	// for persona-update steps (e.g. {"spec":{"resources":{"limits":{"memory":"512Mi"}}}}).
	Patch json.RawMessage `json:"patch,omitempty"`

	// Command is a ready-to-run kubectl command that carries out an advisory
	// step, when a single command can (e.g. correcting a mistyped image tag).
	// It is only ever printed for a human to run; the proposer filters it
	// through dorguv1.SanitizeStepCommand before persisting it.
	Command string `json:"command,omitempty"`
}

// Planner generates an ordered remediation plan from a RemediationContext.
// It is intentionally separate from the diagnosis-enhancement LLM path
// (llm.Client.EnhanceDiagnosis) — this is remediation planning, not root-cause
// summarization.
type Planner interface {
	// PlanRemediation asks the backing model for an ordered, validated plan.
	// On a hard failure it returns an error so the caller can fall back to the
	// deterministic rule-based proposer.
	PlanRemediation(ctx context.Context, rc RemediationContext) (*RemediationPlan, error)
}
