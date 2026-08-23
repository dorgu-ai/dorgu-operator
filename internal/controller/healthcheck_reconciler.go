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
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/diagnosis"
	"github.com/dorgu-ai/dorgu-operator/internal/events"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
	"github.com/dorgu-ai/dorgu-operator/internal/websocket"
)

const (
	// DefaultReconcileInterval is the default health check reconcile interval.
	DefaultReconcileInterval = 60 * time.Second

	// ResolutionGracePeriod is how long a signal must be absent before auto-resolving.
	ResolutionGracePeriod = 5 * time.Minute

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

// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=incidentmemories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas,verbs=get;list;watch
// +kubebuilder:rbac:groups=dorgu.io,resources=applicationpersonas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationactions,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=remediationactions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=clusterpersonas,verbs=get;list;watch
// +kubebuilder:rbac:groups=dorgu.io,resources=dorguevents,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

// HealthCheckReconciler runs detection and diagnosis on a fixed interval,
// creating and updating IncidentMemory CRDs. It implements manager.Runnable.
type HealthCheckReconciler struct {
	Client            client.Client
	Detection         *detection.Engine
	Diagnosis         *diagnosis.Engine
	EventStore        events.EventStore
	EventEmitter      events.Emitter
	Proposer          remediation.RemediationProposer
	Logger            logr.Logger
	ReconcileInterval time.Duration
	WebSocket         *websocket.Server
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
	r.Logger.V(1).Info("starting reconciliation cycle")

	// 1. Collect all signals from detection engine.
	signals, err := r.Detection.CollectAll(ctx)
	if err != nil {
		r.Logger.Error(err, "failed to collect signals")
		return
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

	// 4. Check for resolved incidents.
	if err := r.resolveCleared(ctx, activeSignalKeys); err != nil {
		r.Logger.Error(err, "failed to resolve cleared incidents")
	}

	// 5. Broadcast aggregate health update via WebSocket.
	if r.WebSocket != nil {
		r.broadcastHealthSummary(ctx)
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
	diagnoses, err := r.Diagnosis.Analyze(ctx, group.Signals)
	if err != nil {
		r.Logger.Error(err, "failed to analyze signals", "group", group.Key)
		return 0, 0
	}

	subject, ok := incidentSubjectFor(group)
	if !ok {
		// Cluster-scoped findings (nodes, control plane) belong to no
		// application. They are diagnosed and logged, but giving them an owner
		// they do not have is the mistake this change exists to undo.
		r.Logger.V(1).Info("skipping cluster-scoped diagnoses: no application to file them against",
			"group", group.Key, "diagnoses", len(diagnoses))
		return 0, 0
	}

	for i := range diagnoses {
		diag := &diagnoses[i]
		attempted++
		if err := r.processDiagnosis(ctx, subject, diag, activeSignalKeys); err != nil {
			discarded++
			r.Logger.Error(err, "failed to process diagnosis",
				"summary", diag.Summary,
				"category", diag.Category,
			)
		}
	}

	return attempted, discarded
}

// incidentSubject is the application an incident is filed against.
type incidentSubject struct {
	// personaRef is written to the incident. On an unattributed subject it
	// names the workload rather than a persona that exists, which is why the
	// unattributed flag is recorded next to it instead of inferred from it.
	personaRef dorguv1.PersonaReference

	// unattributed marks a subject no single persona claimed.
	unattributed bool

	// namespace is where the incident lives.
	namespace string
}

// personaSubject builds the subject for an application Dorgu knows by persona.
func personaSubject(ref dorguv1.PersonaReference) incidentSubject {
	return incidentSubject{personaRef: ref, namespace: ref.Namespace}
}

// incidentSubjectFor derives the subject of a signal group, reporting false
// when the group is about no application at all.
func incidentSubjectFor(group *detection.SignalGroup) (incidentSubject, bool) {
	switch group.Scope {
	case detection.ScopePersona:
		if group.PersonaRef == nil {
			return incidentSubject{}, false
		}
		subject := personaSubject(*group.PersonaRef)
		if subject.namespace == "" {
			subject.namespace = group.Namespace
		}
		return subject, true

	case detection.ScopeUnattributed:
		// Recorded against the workload, and honest about it. An incident
		// Dorgu cannot attribute is still an outage the user needs to see;
		// folding it into a neighbouring app to give it an owner is what
		// poisoned every plan in the clean-room run.
		return incidentSubject{
			personaRef: dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      group.Workload,
				Namespace: group.Namespace,
			},
			unattributed: true,
			namespace:    group.Namespace,
		}, true

	default:
		return incidentSubject{}, false
	}
}

// attribution names the subject's attribution for a label and for the spec.
func (s incidentSubject) attribution() string {
	if s.unattributed {
		return AttributionUnattributed
	}
	return AttributionPersona
}

// incidentNameKey is the persona segment of the generated incident name. An
// unattributed incident says so in its own name, so it can never collide with
// the incident raised once a persona does claim the workload.
func (s incidentSubject) incidentNameKey() string {
	if s.unattributed {
		return s.personaRef.Name + "-unattributed"
	}
	return s.personaRef.Name
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

	// Propose remediation if proposer is configured and incident is not resolved.
	if r.Proposer != nil && incident != nil && incident.Status.Phase != PhaseResolved {
		// Honour a rejection before spending anything. Proposing runs an AI
		// planning call, so re-asking a question the user already answered bills
		// them for saying no (F-07).
		suppressed, rejectionErr := r.rejectionSuppressesProposal(ctx, diag, incident)
		if rejectionErr != nil {
			// Fail closed. Guessing "no rejection" when we cannot tell is the
			// expensive direction to be wrong in.
			r.Logger.Error(rejectionErr, "could not read the rejection history; not proposing a remediation",
				"incident", incident.Name)
			return nil
		}
		if suppressed != "" {
			r.Logger.V(1).Info("remediation not proposed", "reason", suppressed, "incident", incident.Name)
			return nil
		}

		result, proposeErr := r.Proposer.Propose(ctx, *diag, incident)
		if proposeErr != nil {
			r.Logger.Error(proposeErr, "failed to propose remediation", "incident", incident.Name)
		} else if result.Proposed {
			r.Logger.Info("remediation proposed", "action", result.Action.Name, "incident", incident.Name)
			// Broadcast remediation creation via WebSocket.
			if r.WebSocket != nil && result.Action != nil {
				r.WebSocket.BroadcastRemediation(websocket.RemediationEvent{
					EventType:   "created",
					Name:        result.Action.Name,
					Namespace:   result.Action.Namespace,
					ActionType:  result.Action.Spec.Action.Type,
					Phase:       result.Action.Status.Phase,
					Confidence:  result.Action.Spec.Confidence,
					PersonaName: result.Action.Spec.PersonaRef.Name,
					PersonaKind: result.Action.Spec.PersonaRef.Kind,
				})
			}
		} else if result.SkipReason != "" {
			r.Logger.V(1).Info("remediation skipped", "reason", result.SkipReason, "incident", incident.Name)
		}
	}

	return nil
}

// findMatchingIncident searches for an active IncidentMemory matching a
// diagnosis about a subject. Attribution is part of the match: an unattributed
// incident and the incident raised once a persona claims the same workload are
// two different records, and neither may be mistaken for the other.
func (r *HealthCheckReconciler) findMatchingIncident(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
) (*dorguv1.IncidentMemory, error) {
	primarySignal := primarySignalType(diag)

	labelSelector := labels.SelectorFromSet(labels.Set{
		LabelPersonaKind: subject.personaRef.Kind,
		LabelPersonaName: subject.personaRef.Name,
		LabelCategory:    diag.Category,
		LabelSignal:      string(primarySignal),
	})

	var list dorguv1.IncidentMemoryList
	opts := []client.ListOption{
		client.MatchingLabelsSelector{Selector: labelSelector},
	}
	if subject.namespace != "" {
		opts = append(opts, client.InNamespace(subject.namespace))
	}

	if err := r.Client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing IncidentMemories: %w", err)
	}

	for i := range list.Items {
		im := &list.Items[i]
		if im.Status.Phase == PhaseResolved || !subject.matchesIncident(im) {
			continue
		}
		return im, nil
	}

	return nil, nil
}

// matchesIncident reports whether an existing incident is the same record this
// subject would write.
//
// Attribution has to agree: an unattributed incident and the incident raised
// once a persona claims the same workload are two different records. An
// incident written before the field existed states nothing, and adopting it is
// better than raising a duplicate beside it, so it is treated as the
// persona-attributed record it was; applyDiagnosisToSpec backfills the field on
// the way through.
func (s incidentSubject) matchesIncident(im *dorguv1.IncidentMemory) bool {
	if im.Spec.Attribution == "" {
		return !s.unattributed
	}
	return im.Spec.Attribution == s.attribution()
}

// updateExistingIncident updates an active IncidentMemory with new diagnosis data.
//
// Both writes retry with a re-fetch, on Conflict and on NotFound alike. The
// object handed in comes from a List, so its ResourceVersion is a snapshot: any
// concurrent write (the incident controller stamping conditions, the
// remediation controller resolving) invalidates it and a bare Update fails with
// "the object has been modified". NotFound is the second half, and the half
// CF4-2 missed: reads come from the manager's cache, which can lag a write the
// API server has already accepted, and a Get that lands in that gap used to end
// the diagnosis then and there (F-05).
func (r *HealthCheckReconciler) updateExistingIncident(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	now metav1.Time,
) error {
	specErr := retryIncidentWrite(func(int) error {
		var fresh dorguv1.IncidentMemory
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), &fresh); err != nil {
			return err
		}
		applyDiagnosisToSpec(&fresh, subject, diag, now)
		if err := r.Client.Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.DeepCopyInto(im)
		return nil
	})
	if specErr != nil {
		return r.reportDiscardedDiagnosis(ctx, im, diag, "recording the root cause", specErr)
	}

	// Update the status subresource the same way. Re-fetching inside the loop
	// ensures we always carry the latest ResourceVersion even if a concurrent
	// controller wrote to the object between attempts.
	err := retryIncidentWrite(func(int) error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
			return err
		}
		im.Status.OccurrenceCount++
		im.Status.LastOccurrence = &now
		return r.Client.Status().Update(ctx, im)
	})
	if err != nil {
		return r.reportDiscardedDiagnosis(ctx, im, diag, "recording the occurrence count", err)
	}

	r.Logger.V(1).Info("updated existing incident",
		"name", im.Name,
		"occurrenceCount", im.Status.OccurrenceCount,
	)

	// Broadcast incident update via WebSocket.
	if r.WebSocket != nil {
		summary := ""
		if im.Spec.RootCause != nil {
			summary = im.Spec.RootCause.Summary
		}
		r.WebSocket.BroadcastIncident(websocket.IncidentEvent{
			EventType:   "updated",
			Name:        im.Name,
			Namespace:   im.Namespace,
			Severity:    im.Spec.Severity,
			Category:    im.Spec.Category,
			Signal:      im.Spec.Detection.Signal,
			Phase:       im.Status.Phase,
			PersonaName: im.Spec.PersonaRef.Name,
			PersonaKind: im.Spec.PersonaRef.Kind,
			Summary:     summary,
		})
	}

	return nil
}

// createIncident creates a new IncidentMemory CRD from a diagnosis.
func (r *HealthCheckReconciler) createIncident(
	ctx context.Context,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	now metav1.Time,
) error {
	primarySignal := primarySignalType(diag)
	namespace := subject.namespace
	if namespace == "" {
		namespace = "default"
	}

	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateIncidentName(namespace, subject.incidentNameKey(), string(primarySignal)),
			Namespace: namespace,
			Labels: map[string]string{
				LabelPersonaKind:      subject.personaRef.Kind,
				LabelPersonaName:      subject.personaRef.Name,
				LabelPersonaNamespace: namespace,
				LabelCategory:         diag.Category,
				LabelSeverity:         string(diag.Severity),
				LabelSignal:           string(primarySignal),
				LabelPhase:            PhaseDetected,
				LabelAttribution:      subject.attribution(),
			},
		},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef:  subject.personaRef,
			Attribution: subject.attribution(),
			Category:    diag.Category,
			Severity:    string(diag.Severity),
			Detection: dorguv1.DetectionInfo{
				Signal:            string(primarySignal),
				Source:            diag.Provider,
				FirstSeen:         now,
				LastSeen:          now,
				AffectedResources: toResourceRefs(diag.AffectedResources),
			},
			RootCause: annotateUnattributed(buildRootCause(diag), subject),
		},
	}

	switch createErr := r.Client.Create(ctx, im); {
	case createErr == nil:
	case apierrors.IsAlreadyExists(createErr):
		// The incident is already there and our cached List did not see it yet.
		// Fold this diagnosis into the object that really exists rather than
		// treating a stale read as a reason to throw away a paid-for analysis
		// (F-05).
		var existing dorguv1.IncidentMemory
		getErr := retryIncidentWrite(func(int) error {
			return r.Client.Get(ctx, client.ObjectKeyFromObject(im), &existing)
		})
		if getErr != nil {
			return r.reportDiscardedDiagnosis(ctx, im, diag,
				"adopting the incident that already existed", getErr)
		}
		r.Logger.V(1).Info("adopted an incident the cache had not caught up with", "name", im.Name)
		return r.updateExistingIncident(ctx, &existing, subject, diag, now)
	default:
		return fmt.Errorf("creating IncidentMemory: %w", createErr)
	}

	// Set the initial status, retrying on Conflict and on NotFound. The first
	// attempt writes through the object Create just returned, which already
	// carries a valid ResourceVersion, so the common path never reads at all.
	// Re-reading unconditionally is what exposed the write to a cache that had
	// not caught up: the Get returned "not found" for an object the API server
	// had just accepted, retry.RetryOnConflict did not consider that worth
	// another try, and the diagnosis died there. Five of the first six
	// diagnoses in a fresh install were lost this way (F-05).
	statusErr := retryIncidentWrite(func(attempt int) error {
		if attempt > 0 {
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
				return err
			}
		}
		im.Status.Phase = PhaseDetected
		im.Status.OccurrenceCount = 1
		im.Status.LastOccurrence = &now
		return r.Client.Status().Update(ctx, im)
	})
	if statusErr != nil {
		// The object exists but carries no status. Surface the loss rather than
		// leaving an orphan nobody knows about; updateExistingIncident repairs
		// the status on the next cycle.
		return r.reportDiscardedDiagnosis(ctx, im, diag, "setting the initial incident status", statusErr)
	}

	r.Logger.Info("created incident",
		"name", im.Name,
		"category", diag.Category,
		"severity", diag.Severity,
		"signal", primarySignal,
		"attribution", subject.attribution(),
	)

	// Broadcast incident creation via WebSocket.
	if r.WebSocket != nil {
		summary := ""
		if diag.Summary != "" {
			summary = diag.Summary
		}
		r.WebSocket.BroadcastIncident(websocket.IncidentEvent{
			EventType:   "created",
			Name:        im.Name,
			Namespace:   im.Namespace,
			Severity:    string(diag.Severity),
			Category:    diag.Category,
			Signal:      string(primarySignal),
			Phase:       PhaseDetected,
			PersonaName: subject.personaRef.Name,
			PersonaKind: subject.personaRef.Kind,
			Summary:     summary,
		})
	}

	// Store contributing signals as DorguEvents.
	r.storeSignalEvents(ctx, diag, im)

	// Emit K8s Events for high-severity signals.
	if diag.Severity == detection.SeverityWarning || diag.Severity == detection.SeverityCritical {
		r.emitK8sEvents(ctx, diag)
	}

	return nil
}

// resolveCleared closes the incidents whose applications can be shown to have
// recovered, and leaves every other incident open.
//
// The old rule was two absences: no matching signal this cycle, and a grace
// period since the last one. Neither is evidence of anything. A crash loop
// backs off in lengthening intervals up to five minutes, so a pod that is
// completely dead falls silent inside the grace period and reads as fixed. That
// is how platform/checkout reached 51 occurrences, went Resolved, and stayed in
// CrashLoopBackOff, while dorgu health reported one active incident with three
// applications down (F-01).
//
// Silence now only opens the question. verifyRecovery has to answer it with
// something observed: pods that exist, are Ready, and have stayed Ready without
// restarting for RecoveryStabilityWindow. Anything else, including any error
// reading the cluster, leaves the incident open.
func (r *HealthCheckReconciler) resolveCleared(ctx context.Context, activeSignalKeys map[string]bool) error {
	// Use label selector to only fetch non-resolved incidents.
	var list dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &list,
		client.MatchingLabels{LabelPhase: PhaseDetected},
	); err != nil {
		return fmt.Errorf("listing active IncidentMemories: %w", err)
	}

	// Also check Investigating phase incidents.
	var investigatingList dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &investigatingList,
		client.MatchingLabels{LabelPhase: PhaseInvestigating},
	); err != nil {
		return fmt.Errorf("listing investigating IncidentMemories: %w", err)
	}
	list.Items = append(list.Items, investigatingList.Items...)

	now := time.Now()
	for i := range list.Items {
		im := &list.Items[i]

		// Build signal key from incident fields.
		key := incidentSignalKey(im)
		if activeSignalKeys[key] {
			continue // Still active.
		}

		// Check grace period: signal must be absent for ResolutionGracePeriod.
		if now.Sub(im.Spec.Detection.LastSeen.Time) < ResolutionGracePeriod {
			continue
		}

		evidence := r.resolutionEvidence(ctx, im, now)
		if !evidence.Recovered() {
			r.Logger.V(1).Info("incident stays open: recovery could not be established",
				"name", im.Name,
				"signal", im.Spec.Detection.Signal,
				"reason", evidence.Reason,
			)
			continue
		}

		if err := r.markResolved(ctx, im, evidence); err != nil {
			r.Logger.Error(err, "failed to resolve incident", "name", im.Name)
			continue
		}

		r.Logger.Info("auto-resolved incident",
			"name", im.Name,
			"category", im.Spec.Category,
			"signal", im.Spec.Detection.Signal,
			"lastSeen", im.Spec.Detection.LastSeen.Time,
			"evidence", evidence.Reason,
		)

		// Broadcast incident resolution via WebSocket.
		if r.WebSocket != nil {
			r.WebSocket.BroadcastIncident(websocket.IncidentEvent{
				EventType:   "resolved",
				Name:        im.Name,
				Namespace:   im.Namespace,
				Severity:    im.Spec.Severity,
				Category:    im.Spec.Category,
				Signal:      im.Spec.Detection.Signal,
				Phase:       PhaseResolved,
				PersonaName: im.Spec.PersonaRef.Name,
				PersonaKind: im.Spec.PersonaRef.Kind,
			})
		}
	}

	return nil
}

// resolutionEvidence is the full test an incident must pass to close: either its
// workload has been observed healthy, or the incident has been superseded by
// one filed against a persona that now claims the same workload.
func (r *HealthCheckReconciler) resolutionEvidence(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	now time.Time,
) recoveryEvidence {
	if evidence, superseded := r.supersededByPersona(ctx, im); superseded {
		return evidence
	}
	return r.verifyRecovery(ctx, im, now)
}

// supersededByPersona closes an unattributed incident once an attributed one is
// already tracking the same workload, so that onboarding an app mid-outage does
// not leave it counted twice.
//
// The test is the replacement incident, not the persona. A persona existing
// only means the workload could be attributed from now on; it says nothing
// about whether the outage is still recorded anywhere. Closing on that alone
// would hand back exactly the failure this round exists to remove: a broken
// application with no open incident. So this hands over only when there is
// something to hand over to, and the resolution it writes says handover rather
// than recovery, because nothing here observed the workload at all.
func (r *HealthCheckReconciler) supersededByPersona(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
) (recoveryEvidence, bool) {
	if im.Spec.Attribution != AttributionUnattributed {
		return recoveryEvidence{}, false
	}

	namespace := incidentNamespace(im)
	if namespace == "" {
		return recoveryEvidence{}, false
	}
	workloadName := incidentWorkloadName(im)

	var attributed dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &attributed,
		client.InNamespace(namespace),
		client.MatchingLabels{
			LabelPersonaKind: "ApplicationPersona",
			LabelPersonaName: workloadName,
			LabelAttribution: AttributionPersona,
		},
	); err != nil {
		r.Logger.V(1).Info("could not check whether an attributed incident now covers the workload",
			"incident", im.Name, "error", err)
		return recoveryEvidence{}, false
	}

	for i := range attributed.Items {
		other := &attributed.Items[i]
		if other.Name == im.Name || other.Status.Phase == PhaseResolved {
			continue
		}
		return healthy(fmt.Sprintf(
			"superseded: workload %s is now tracked by incident %s, which is open against its persona",
			workloadName, other.Name)), true
	}

	return recoveryEvidence{}, false
}

// markResolved writes the resolution and the phase, recording on the object
// itself what was observed. Both writes retry on Conflict and on NotFound: this
// runs alongside the incident and remediation controllers, and a resolution
// dropped on a stale ResourceVersion leaves an incident that reads as open for
// an application that is fine.
func (r *HealthCheckReconciler) markResolved(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	evidence recoveryEvidence,
) error {
	specErr := retryIncidentWrite(func(int) error {
		var fresh dorguv1.IncidentMemory
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), &fresh); err != nil {
			return err
		}
		if fresh.Labels == nil {
			fresh.Labels = map[string]string{}
		}
		fresh.Labels[LabelPhase] = PhaseResolved
		fresh.Spec.Resolution = &dorguv1.ResolutionInfo{
			Action:  resolutionAction(evidence),
			Outcome: "resolved",
		}
		if err := r.Client.Update(ctx, &fresh); err != nil {
			return err
		}
		fresh.DeepCopyInto(im)
		return nil
	})
	if specErr != nil {
		return fmt.Errorf("recording the resolution: %w", specErr)
	}

	statusErr := retryIncidentWrite(func(int) error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
			return err
		}
		im.Status.Phase = PhaseResolved
		return r.Client.Status().Update(ctx, im)
	})
	if statusErr != nil {
		return fmt.Errorf("recording the resolved phase: %w", statusErr)
	}

	return nil
}

// broadcastHealthSummary collects aggregate health data and broadcasts it.
func (r *HealthCheckReconciler) broadcastHealthSummary(ctx context.Context) {
	// Count active incidents.
	var activeIncidents dorguv1.IncidentMemoryList
	activeCount := 0
	if err := r.Client.List(ctx, &activeIncidents,
		client.MatchingLabels{LabelPhase: PhaseDetected},
	); err == nil {
		activeCount += len(activeIncidents.Items)
	}
	var investigatingIncidents dorguv1.IncidentMemoryList
	if err := r.Client.List(ctx, &investigatingIncidents,
		client.MatchingLabels{LabelPhase: PhaseInvestigating},
	); err == nil {
		activeCount += len(investigatingIncidents.Items)
	}

	// Count pending remediations.
	var pendingRemedies dorguv1.RemediationActionList
	pendingCount := 0
	if err := r.Client.List(ctx, &pendingRemedies); err == nil {
		for i := range pendingRemedies.Items {
			if pendingRemedies.Items[i].Status.Phase == "Pending" {
				pendingCount++
			}
		}
	}

	r.WebSocket.BroadcastHealthUpdate(websocket.HealthUpdateEvent{
		EventType:       "health-update",
		ActiveIncidents: activeCount,
		PendingRemedies: pendingCount,
	})
}

// storeSignalEvents stores contributing signals as DorguEvents via the event store.
func (r *HealthCheckReconciler) storeSignalEvents(ctx context.Context, diag *diagnosis.Diagnosis, im *dorguv1.IncidentMemory) {
	for _, cs := range diag.Contributing {
		internalEvent := &events.InternalEvent{
			ID:       fmt.Sprintf("%s-%s-%d", im.Name, cs.Signal.Type, cs.Signal.DetectedAt.UnixNano()),
			Severity: events.Severity(cs.Signal.Severity),
			Category: events.Category(cs.Signal.Category),
			Source:   cs.Signal.Source,
			Message:  cs.Signal.Message,
			InvolvedObject: dorguv1.ResourceReference{
				Kind:      cs.Signal.Resource.Kind,
				Name:      cs.Signal.Resource.Name,
				Namespace: cs.Signal.Resource.Namespace,
			},
			PersonaRef: cs.Signal.PersonaRef,
			EventTime:  cs.Signal.DetectedAt,
		}

		if err := r.EventStore.Store(ctx, internalEvent); err != nil {
			r.Logger.Error(err, "failed to store signal event",
				"signal", cs.Signal.Type,
				"resource", cs.Signal.Resource.Name,
			)
		}
	}
}

// emitK8sEvents emits K8s Events for diagnosis contributing signals.
func (r *HealthCheckReconciler) emitK8sEvents(ctx context.Context, diag *diagnosis.Diagnosis) {
	for _, cs := range diag.Contributing {
		internalEvent := &events.InternalEvent{
			Severity: events.Severity(cs.Signal.Severity),
			Category: events.Category(cs.Signal.Category),
			Source:   cs.Signal.Source,
			Message:  fmt.Sprintf("%s: %s", diag.Summary, cs.Signal.Message),
			InvolvedObject: dorguv1.ResourceReference{
				Kind:      cs.Signal.Resource.Kind,
				Name:      cs.Signal.Resource.Name,
				Namespace: cs.Signal.Resource.Namespace,
			},
			PersonaRef: cs.Signal.PersonaRef,
			EventTime:  cs.Signal.DetectedAt,
		}

		if err := r.EventEmitter.Emit(ctx, internalEvent); err != nil {
			r.Logger.Error(err, "failed to emit K8s event",
				"signal", cs.Signal.Type,
				"resource", cs.Signal.Resource.Name,
			)
		}
	}
}

// signalKey creates a unique key for a diagnosis to track active signals. It is
// keyed on the subject rather than the diagnosis, so it matches whatever was
// written to the incident, including for an unattributed subject where the
// diagnosis itself carries no persona.
func (r *HealthCheckReconciler) signalKey(subject incidentSubject, diag *diagnosis.Diagnosis) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		subject.personaRef.Kind,
		subject.personaRef.Namespace,
		subject.personaRef.Name,
		diag.Category,
		primarySignalType(diag),
	)
}

// incidentSignalKey reconstructs the signal key from an IncidentMemory.
func incidentSignalKey(im *dorguv1.IncidentMemory) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		im.Spec.PersonaRef.Kind,
		im.Spec.PersonaRef.Namespace,
		im.Spec.PersonaRef.Name,
		im.Spec.Category,
		im.Spec.Detection.Signal,
	)
}

// primarySignalType returns the primary signal type from a diagnosis.
func primarySignalType(diag *diagnosis.Diagnosis) detection.SignalType {
	if len(diag.Contributing) > 0 {
		return diag.Contributing[0].Signal.Type
	}
	return detection.SignalType("Unknown")
}

// generateIncidentName creates a deterministic name for an IncidentMemory.
// Format: im-{namespace}-{persona}-{signal}-{hash}
// Truncates variable segments to ensure the hash suffix is always preserved.
func generateIncidentName(namespace, persona, signal string) string {
	hashInput := fmt.Sprintf("%s/%s/%s", namespace, persona, signal)
	hash := sha256.Sum256([]byte(hashInput))
	hashStr := fmt.Sprintf("%x", hash[:6])

	ns := sanitizeName(namespace)
	p := sanitizeName(persona)
	sig := sanitizeName(signal)

	// Reserve space for overhead, distribute remaining among segments.
	maxSegmentLen := (MaxIncidentNameLength - incidentNameOverhead) / 3
	ns = truncate(ns, maxSegmentLen)
	p = truncate(p, maxSegmentLen)
	sig = truncate(sig, maxSegmentLen)

	return fmt.Sprintf("im-%s-%s-%s-%s", ns, p, sig, hashStr)
}

// truncate returns s truncated to maxLen characters.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// sanitizeName converts a string to a valid K8s name component.
func sanitizeName(s string) string {
	result := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32)
		} else {
			result = append(result, '-')
		}
	}
	return string(result)
}

// applyDiagnosisToSpec folds a diagnosis into an IncidentMemory spec. It is
// separate from the write so the retry loop can re-apply it to a freshly fetched
// object instead of replaying a stale one.
func applyDiagnosisToSpec(
	im *dorguv1.IncidentMemory,
	subject incidentSubject,
	diag *diagnosis.Diagnosis,
	now metav1.Time,
) {
	im.Spec.Detection.LastSeen = now

	// Backfill attribution on incidents raised before it existed, and on any
	// incident adopted after an AlreadyExists. Without it findMatchingIncident
	// would stop recognising the record and raise a duplicate every cycle.
	if im.Spec.Attribution == "" {
		im.Spec.Attribution = subject.attribution()
	}
	if im.Labels == nil {
		im.Labels = map[string]string{}
	}
	if im.Labels[LabelAttribution] == "" {
		im.Labels[LabelAttribution] = im.Spec.Attribution
	}

	// Refresh the root cause when the new diagnosis is at least as confident as
	// the recorded one. The tie matters: an AI-enhanced diagnosis carries the
	// same numeric confidence as the rule-based diagnosis it enhances, so
	// requiring a strict improvement is how an incident first recorded before AI
	// was configured stayed stamped "rule-based" for its whole life (F-05).
	if diag.Confidence > 0 {
		existingConfidence := 0.0
		if im.Spec.RootCause != nil {
			parsed, err := strconv.ParseFloat(im.Spec.RootCause.Confidence, 64)
			if err == nil {
				existingConfidence = parsed
			}
		}
		if diag.Confidence >= existingConfidence {
			im.Spec.RootCause = annotateUnattributed(buildRootCause(diag), subject)
		}
	}

	im.Spec.Detection.AffectedResources = toResourceRefs(diag.AffectedResources)
}

// annotateUnattributed adds, to the field a user actually reads, the reason an
// incident names a workload instead of an application. Dorgu can see the
// workload is broken and can say nothing more useful than that, so it says
// exactly that rather than presenting a persona-shaped record with no persona
// behind it.
func annotateUnattributed(rc *dorguv1.RootCauseInfo, subject incidentSubject) *dorguv1.RootCauseInfo {
	if rc == nil || !subject.unattributed {
		return rc
	}

	rc.Summary = strings.TrimSpace(rc.Summary) + fmt.Sprintf(
		" No ApplicationPersona in namespace %s claims workload %s, so this incident is recorded against the workload"+
			" and Dorgu will not propose a remediation for it. Run \"dorgu persona import -n %s\" to onboard it.",
		subject.namespace, subject.personaRef.Name, subject.namespace)

	return rc
}

// reportDiscardedDiagnosis makes a lost diagnosis loud, then returns the error
// so the cycle counts it.
//
// A diagnosis is not free: the AI path bills a model call for every one. Losing
// one to a write that could not be retried is a real cost to the user, so it is
// logged at ERROR, recorded as a DorguEvent, and emitted as a Kubernetes Warning
// on the incident. Whatever the operator's failure mode, the user finds out.
func (r *HealthCheckReconciler) reportDiscardedDiagnosis(
	ctx context.Context,
	im *dorguv1.IncidentMemory,
	diag *diagnosis.Diagnosis,
	stage string,
	cause error,
) error {
	err := fmt.Errorf("%s for IncidentMemory %s: %w", stage, im.Name, cause)

	persona := ""
	if diag.PersonaRef != nil {
		persona = diag.PersonaRef.Name
	}

	r.Logger.Error(err, "diagnosis discarded: could not persist it after retrying on conflict",
		"incident", im.Name,
		"namespace", im.Namespace,
		"persona", persona,
		"provider", diag.Provider,
		"confidence", diag.Confidence,
		"summary", diag.Summary,
	)

	message := fmt.Sprintf(
		"discarded the%s diagnosis for %s after %s failed: %v. The analysis was produced and paid for but is not recorded.",
		providerSuffix(diag.Provider), im.Name, stage, cause)

	surfaced := &events.InternalEvent{
		ID:       fmt.Sprintf("diagnosis-discarded-%s-%d", im.Name, time.Now().UnixNano()),
		Severity: events.SeverityCritical,
		Category: events.Category(im.Spec.Category),
		Source:   "healthcheck-reconciler",
		Reason:   ReasonDiagnosisDiscarded,
		Message:  message,
		InvolvedObject: dorguv1.ResourceReference{
			Kind:      "IncidentMemory",
			Name:      im.Name,
			Namespace: im.Namespace,
		},
		PersonaRef: diag.PersonaRef,
		EventTime:  time.Now(),
	}

	if r.EventStore != nil {
		if storeErr := r.EventStore.Store(ctx, surfaced); storeErr != nil {
			r.Logger.Error(storeErr, "could not record the discarded diagnosis as a DorguEvent",
				"incident", im.Name)
		}
	}
	if r.EventEmitter != nil {
		if emitErr := r.EventEmitter.Emit(ctx, surfaced); emitErr != nil {
			r.Logger.Error(emitErr, "could not emit a Kubernetes event for the discarded diagnosis",
				"incident", im.Name)
		}
	}

	return err
}

// providerSuffix names the diagnosis source in a user-facing message, and says
// nothing when the source is unknown rather than inventing one.
func providerSuffix(provider string) string {
	if provider == "" {
		return ""
	}
	return " " + provider
}

// buildRootCause converts a diagnosis into an IncidentMemory RootCauseInfo.
func buildRootCause(diag *diagnosis.Diagnosis) *dorguv1.RootCauseInfo {
	if diag.Confidence == 0 && diag.Summary == "" {
		return nil
	}

	contributing := make([]dorguv1.ContributingSignal, 0, len(diag.Contributing))
	for _, cs := range diag.Contributing {
		contributing = append(contributing, dorguv1.ContributingSignal{
			Signal: string(cs.Signal.Type),
			Detail: cs.Detail,
		})
	}

	return &dorguv1.RootCauseInfo{
		Summary:      diag.Summary,
		Confidence:   fmt.Sprintf("%.2f", diag.Confidence),
		Provider:     diag.Provider,
		Contributing: contributing,
	}
}

// toResourceRefs converts affected resources, preserving the "affected" role.
func toResourceRefs(refs []dorguv1.ResourceReference) []dorguv1.ResourceReference {
	result := make([]dorguv1.ResourceReference, len(refs))
	for i, ref := range refs {
		result[i] = dorguv1.ResourceReference{
			Kind:      ref.Kind,
			Name:      ref.Name,
			Namespace: ref.Namespace,
			Role:      "affected",
		}
	}
	return result
}

// timeEqual compares two *metav1.Time values for equality.
func timeEqual(a, b *metav1.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(b)
}
