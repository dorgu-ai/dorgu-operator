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

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
	"github.com/dorgu-ai/dorgu-operator/internal/metrics"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

const (
	// DefaultReconcileInterval is the default health check reconcile interval.
	DefaultReconcileInterval = 60 * time.Second

	// ResolutionGracePeriod is how long a signal must be absent before auto-resolving.
	ResolutionGracePeriod = 5 * time.Minute

	// DefaultRecurrenceCap and DefaultRecurrenceWindow: once a fault has opened this
	// many recurrences inside the window, the next one reopens the latest incident
	// instead of creating another. Three in a day is a fault that flaps, not one
	// that came back.
	DefaultRecurrenceCap    = 3
	DefaultRecurrenceWindow = 24 * time.Hour

	// DefaultResolvedRetention is how long a Resolved incident is kept before it
	// may be pruned.
	DefaultResolvedRetention = 30 * 24 * time.Hour

	// MaxIncidentNameLength is the maximum length for IncidentMemory names.
	MaxIncidentNameLength = 253

	// incidentNameOverhead is the fixed overhead in generated names:
	// "im-" (3) + 3 separators ("-") + hash (12) = 18 bytes.
	incidentNameOverhead = 18

	// Label keys for efficient IncidentMemory lookups.
	LabelPersonaKind      = "dorgu.io/persona-kind"
	LabelPersonaName      = "dorgu.io/persona-name"
	LabelPersonaNamespace = "dorgu.io/persona-namespace"
	LabelCategory         = "dorgu.io/category"
	LabelSeverity         = "dorgu.io/severity"
	LabelSignal           = "dorgu.io/signal"
	LabelPhase            = "dorgu.io/phase"

	// LabelAttribution records whether an incident could be tied to a persona,
	// so "what does Dorgu not understand about this cluster?" is one query.
	LabelAttribution = "dorgu.io/attribution"

	// Attribution values. AttributionUnattributed means the signals were real
	// but no single persona claimed them, so personaRef names the workload
	// rather than a persona that exists.
	AttributionPersona      = "persona"
	AttributionUnattributed = "unattributed"

	// AnnotationRecurrenceOf names the Resolved incident this one is a recurrence
	// of, and AnnotationRecurrenceCount says how many earlier incidents of the
	// same fault the cluster has recorded (1 for the first recurrence).
	AnnotationRecurrenceOf    = "dorgu.io/recurrence-of"
	AnnotationRecurrenceCount = "dorgu.io/recurrence-count"

	// AnnotationEscalatedFrom lists, in order, the signals an incident had before it
	// was escalated to its current one (see incident_family.go).
	AnnotationEscalatedFrom = "dorgu.io/escalated-from"

	// AnnotationLastResolution keeps the resolution of an incident that was reopened
	// (spec.resolution is cleared on an open incident).
	AnnotationLastResolution = "dorgu.io/last-resolution"

	// AnnotationContainers lists the containers an incident is about, the second
	// part of the identity of a fault (application, container, family).
	AnnotationContainers = "dorgu.io/containers"

	// maxEscalationTrail bounds the escalated-from annotation: a fault that
	// flickers between shapes keeps the most recent entries.
	maxEscalationTrail = 16

	// AnnotationRecurrencesFolded counts the recurrences that were folded into an
	// incident (and reopened it) because the recurrence cap was reached.
	AnnotationRecurrencesFolded = "dorgu.io/recurrences-folded"

	// EventReasonIncidentFlapping is the Event reason for the one Warning an
	// incident gets when the recurrence cap is first reached.
	EventReasonIncidentFlapping = "DorguIncidentFlapping"

	// IncidentMemory phase constants.
	PhaseDetected      = "Detected"
	PhaseInvestigating = "Investigating"
	PhaseResolved      = "Resolved"

	// ReasonDiagnosisDiscarded is the Kubernetes Event reason for a diagnosis the
	// operator produced but could not persist. It is deliberately distinct from
	// the detection reason: this is dorgu reporting its own failure, not a
	// finding about the cluster.
	ReasonDiagnosisDiscarded = "DorguDiagnosisDiscarded"
)

// `delete` on incidentmemories is pruning Resolved incidents past the retention: dorgu's
// own records, never a workload (see incident_prune.go). Verify both halves:
//
//	kubectl auth can-i delete incidentmemories.dorgu.io --as=system:serviceaccount:dorgu-system:dorgu-operator -A
//	kubectl auth can-i delete deployments --as=system:serviceaccount:dorgu-system:dorgu-operator -A
// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas,verbs=get;list;watch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationactions,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationactions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=clusterpersonas,verbs=get;list;watch
// `delete` on dorguevents is the retention and cap cleaner pruning dorgu's own
// records. It is scoped to dorgu.io/dorguevents and to no workload kind, so the
// published claim that the operator never writes workloads is untouched. Verify
// it rather than take it on trust:
//
//	kubectl auth can-i delete dorguevents.dorgu.io --as=system:serviceaccount:dorgu-system:dorgu-operator -A
//	kubectl auth can-i delete deployments --as=system:serviceaccount:dorgu-system:dorgu-operator -A
// +kubebuilder:rbac:groups=dorgu.io,resources=dorguevents,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// The control-plane collector reads the scheduler and controller-manager
// Leases through the cached client, which starts a cluster-wide informer.
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

// HealthCheckReconciler runs detection and diagnosis on a fixed interval,
// creating and updating IncidentMemory CRDs. It implements manager.Runnable.
type HealthCheckReconciler struct {
	Client       client.Client
	Detection    *detection.Engine
	Diagnosis    *diagnosis.Engine
	EventStore   events.EventStore
	EventEmitter events.Emitter
	Proposer     remediation.RemediationProposer

	// Recorder emits the Kubernetes Events for proposal-time refusals. Nil records
	// none.
	Recorder          record.EventRecorder
	Logger            logr.Logger
	ReconcileInterval time.Duration

	// RecurrenceCap and RecurrenceWindow bound how many IncidentMemories one
	// flapping fault can open: after RecurrenceCap recurrences inside
	// RecurrenceWindow the latest incident is reopened instead. Zero disables the
	// cap.
	RecurrenceCap    int
	RecurrenceWindow time.Duration

	// ResolvedRetention is how long a Resolved incident is kept. Zero keeps them
	// forever.
	ResolvedRetention time.Duration

	// APIReader reads straight from the API server, bypassing the cache. Pruning
	// decides from the cache but confirms against this before it deletes. Nil reads
	// through Client.
	APIReader client.Reader
}

// Start begins the health check reconciliation loop. Blocks until ctx is cancelled.
// Implements manager.Runnable.
func (r *HealthCheckReconciler) Start(ctx context.Context) error {
	interval := r.ReconcileInterval
	if interval == 0 {
		interval = DefaultReconcileInterval
	}

	r.Logger.Info("starting health check reconciler", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once immediately on startup.
	r.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			r.Logger.Info("health check reconciler stopped")
			return nil
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

// reconcile runs one detection→diagnosis→incident cycle.
func (r *HealthCheckReconciler) reconcile(ctx context.Context) {
	timeCycle(func() { r.runCycle(ctx) })
}

// timeCycle runs one health-check cycle and observes its duration.
func timeCycle(cycle func()) {
	started := time.Now()
	defer func() { metrics.RecordHealthCycle(time.Since(started)) }()
	cycle()
}

// runCycle is one cycle of the loop: collect, diagnose, record, propose.
func (r *HealthCheckReconciler) runCycle(ctx context.Context) {
	r.Logger.V(1).Info("starting reconciliation cycle")

	// 0. A Pending proposal whose incident has Resolved is stale advice and would
	// defer the next proposal for the same fault. Settle those first, so a
	// recurrence noticed below is proposed for in this cycle (B10 a).
	if err := r.expireResolvedPending(ctx); err != nil {
		logFailure(r.Logger, err, "failed to expire proposals for resolved incidents")
	}

	// 1. Collect all signals from detection engine. A partial failure still
	// yields the signals the working collectors saw, and those are recorded as
	// usual. What a partial cycle cannot do is conclude anything from silence:
	// the collector that failed may be the one that would have reported the
	// incident, so nothing is resolved this cycle (B2b).
	signals, err := r.Detection.CollectAll(ctx)
	incomplete := false
	if err != nil {
		if !detection.IsPartial(err) {
			r.Logger.Error(err, "failed to collect signals")
			return
		}
		incomplete = true
		r.Logger.Error(err, "signal collection incomplete: open incidents are kept and none are resolved this cycle")
	}

	// 2. Split the signals by application before diagnosing anything. Diagnosis
	// used to run once across the whole cluster, which let a single rule
	// describe four unrelated apps in three namespaces as one incident and led
	// the planner to invent a node-level memory pressure event (F-02).
	groups := detection.GroupSignals(signals)

	// 3. Diagnose and record each application on its own.
	activeSignalKeys := make(map[string]bool)
	attempted, discarded := 0, 0
	for i := range groups {
		groupAttempted, groupDiscarded := r.processGroup(ctx, &groups[i], activeSignalKeys)
		attempted += groupAttempted
		discarded += groupDiscarded
	}

	r.Logger.V(1).Info("reconciliation cycle results",
		"signals", len(signals),
		"groups", len(groups),
		"diagnoses", attempted,
	)
	r.logCycleSummary(attempted, discarded)

	// 4. Check for resolved incidents, unless detection was blind to part of the
	// cluster: absence of a signal is only evidence when every collector ran.
	if !incomplete {
		if err := r.resolveCleared(ctx, activeSignalKeys); err != nil {
			r.Logger.Error(err, "failed to resolve cleared incidents")
		}
	}

	// 5. Prune Resolved incidents past the retention. It reads no signals, so a
	// partial collection does not matter to it.
	if err := r.pruneResolved(ctx, time.Now()); err != nil {
		logFailure(r.Logger, err, "failed to prune Resolved incidents")
	}
}

// processGroup diagnoses one application's signals in isolation and records the
// result, returning how many diagnoses it tried to persist and how many it
// lost. A rule handed only one application's signals can only ever produce one
// application's diagnosis, which is the whole reason grouping happens first.
func (r *HealthCheckReconciler) processGroup(
	ctx context.Context,
	group *detection.SignalGroup,
	activeSignalKeys map[string]bool,
) (attempted, discarded int) {
	// Decide who the group is about before diagnosing it. A cluster-scoped group
	// (nodes, control plane) belongs to no application and is dropped below, so
	// diagnosing it would spend a model call on a finding nothing records (H-3).
	subject, ok := incidentSubjectFor(group)
	if !ok {
		r.Logger.V(1).Info("skipping cluster-scoped group: no application to file it against",
			"group", group.Key, "signals", len(group.Signals))
		return 0, 0
	}

	// What this application's signals say right now, for the merge of one fault's
	// shapes into one incident (incident_family.go).
	ctx = withFamilyView(ctx, newFamilyView(group.Signals))

	// An unattributed incident is reported but never remediated (see
	// processDiagnosis), so an AI explanation of it would be paid for and used by
	// nobody: keep the group away from the model. The rules still diagnose it.
	ctx = diagnosis.WithCallOptions(ctx, diagnosis.CallOptions{
		NoModel:             subject.unattributed,
		PreviousOccurrences: r.previousOccurrences(subject),
	})

	diagnoses, err := r.Diagnosis.Analyze(ctx, group.Signals)
	if err != nil {
		r.Logger.Error(err, "failed to analyze signals", "group", group.Key)
		return 0, 0
	}

	for i := range diagnoses {
		diag := &diagnoses[i]
		attempted++
		if err := r.processDiagnosis(ctx, subject, diag, activeSignalKeys); err != nil {
			if isBenignGone(err) {
				// The namespace or incident was deleted mid-cycle. There is nothing
				// left to record against, so it is neither an error nor a lost
				// diagnosis.
				r.Logger.V(1).Info("diagnosis dropped: its namespace or incident is gone",
					"summary", diag.Summary, "reason", err.Error())
				continue
			}
			discarded++
			r.Logger.Error(err, "failed to process diagnosis",
				"summary", diag.Summary,
				"category", diag.Category,
			)
		}
	}

	return attempted, discarded
}

// processPeerDiagnosis records a diagnosis against the incident its fault already
// has, and keeps that incident active for as long as any rung of the fault is seen.
func (r *HealthCheckReconciler) processPeerDiagnosis(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	peer *dorguv1.IncidentMemory,
	activeSignalKeys map[string]bool,
	now metav1.Time,
) error {
	propose, err := r.mergeIntoPeer(ctx, subject, diag, peer, now)
	if err != nil {
		return err
	}
	// The key is read after the merge: an escalation changes the incident's signal.
	activeSignalKeys[incidentSignalKey(peer)] = true

	if !propose || subject.unattributed {
		return nil
	}
	r.proposeRemediation(ctx, diag, peer)
	return nil
}

// previousOccurrences answers, for the diagnosis provider's prompt, how many times
// the incident behind a diagnosis has already been recorded. It reads the same
// IncidentMemory processDiagnosis is about to update, so the model is told the
// real history, and nothing when there is none. A lookup that fails answers 0:
// the prompt then says "first sighting", which is the more cautious claim.
func (r *HealthCheckReconciler) previousOccurrences(subject incidentSubject) func(context.Context, diagnosis.Diagnosis) int32 {
	return func(ctx context.Context, d diagnosis.Diagnosis) int32 {
		existing, err := r.findMatchingIncident(ctx, subject, &d)
		if err != nil {
			return 0
		}
		if existing != nil {
			return existing.Status.OccurrenceCount
		}
		// The fault may be open under another shape of the same family.
		if peer, err := r.findFamilyPeer(ctx, subject, &d); err == nil && peer != nil {
			return peer.Status.OccurrenceCount
		}
		// No open incident: when the fault is back after one resolved, it is not a
		// first sighting, and the model is told how often it was seen before.
		resolved, err := r.resolvedPredecessors(ctx, subject, &d)
		if err != nil {
			return 0
		}
		if previous := latestResolved(resolved); previous != nil {
			return previous.Status.OccurrenceCount
		}
		return 0
	}
}

// logCycleSummary closes each cycle with a count of the diagnoses it could not
// persist. Individual failures are already logged, but nothing ever added them
// up, which is how 176 discarded diagnoses in one 4h20m window went unnoticed
// until someone grepped the raw operator log (F-01). A cycle that lost work says
// so at ERROR; a clean cycle stays quiet at V(1).
func (r *HealthCheckReconciler) logCycleSummary(attempted, discarded int) {
	if discarded == 0 {
		r.Logger.V(1).Info("reconciliation cycle complete", "diagnosesPersisted", attempted)
		return
	}
	r.Logger.Error(
		fmt.Errorf("%d of %d diagnoses could not be persisted", discarded, attempted),
		"diagnoses discarded this cycle; each one cost a diagnosis call and is not recorded anywhere",
		"discarded", discarded,
		"attempted", attempted,
	)
}

// processDiagnosis creates or updates an IncidentMemory for a diagnosis,
// stores events, emits K8s events for high-severity signals, and proposes remediation.
func (r *HealthCheckReconciler) processDiagnosis(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	activeSignalKeys map[string]bool,
) error {
	// Track the signal key for resolution detection.
	signalKey := r.signalKey(subject, diag)
	activeSignalKeys[signalKey] = true

	now := metav1.Now()

	// Check if matching IncidentMemory already exists.
	existing, err := r.findMatchingIncident(ctx, subject, diag)
	if err != nil {
		return fmt.Errorf("finding matching incident: %w", err)
	}

	// A pod Pending only because its image cannot be pulled already has an incident
	// (see incident_image_error.go).
	if existing == nil {
		covering, err := r.imageErrorCovering(ctx, subject, diag)
		if err != nil {
			return fmt.Errorf("finding the image error that explains this pending pod: %w", err)
		}
		if covering != nil {
			return r.foldIntoImageError(ctx, diag, covering, activeSignalKeys, now)
		}
		// And a proposal for the same pod's earlier PodPendingLong incident is advice
		// the image error has overtaken.
		r.supersedePendingLongProposals(ctx, subject, diag)
	}

	// No incident for this signal: the fault may be one the application already has
	// an incident for in another shape (see incident_family.go).
	if existing == nil {
		peer, err := r.findFamilyPeer(ctx, subject, diag)
		if err != nil {
			return fmt.Errorf("finding the incident this fault already has: %w", err)
		}
		if peer != nil {
			return r.processPeerDiagnosis(ctx, subject, diag, peer, activeSignalKeys, now)
		}
	}

	var incident *dorguv1.IncidentMemory
	if existing != nil {
		// Update existing incident.
		if err := r.updateExistingIncident(ctx, existing, subject, diag, now); err != nil {
			return err
		}
		incident = existing
	} else {
		// Create new IncidentMemory.
		if err := r.createIncident(ctx, subject, diag, now); err != nil {
			return err
		}
		// Re-fetch the incident for proposer.
		incident, _ = r.findMatchingIncident(ctx, subject, diag)
	}

	// An unattributed incident has no persona to remediate against, and a plan
	// written against a persona that does not exist is worse than no plan.
	// Report the outage, say what is missing, and stop there.
	if subject.unattributed {
		r.Logger.V(1).Info("remediation not proposed: the workload has no persona",
			"namespace", subject.namespace, "workload", subject.personaRef.Name)
		return nil
	}

	// Propose remediation if a proposer is configured and the incident is open.
	r.proposeRemediation(ctx, diag, incident)

	return nil
}
