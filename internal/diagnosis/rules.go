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

package diagnosis

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

const providerNameRuleBased = "rule-based"

// SuggestedAction values a rule can recommend. They are compared by the engine
// and asserted on by callers, so they live here rather than as loose literals.
const (
	actionResourceAdjustment = "resource-adjustment"
	actionDeploymentFix      = "deployment-fix"
	actionInvestigate        = "investigate"
)

// Rule examines signals and optionally produces a diagnosis.
type Rule struct {
	// Name identifies the rule.
	Name string

	// Match determines if this rule applies to the given signals.
	Match func(signals []detection.Signal) bool

	// Diagnose produces a diagnosis from matching signals.
	Diagnose func(signals []detection.Signal) *Diagnosis
}

// RuleBasedProvider is the deterministic diagnosis provider.
type RuleBasedProvider struct {
	rules  []Rule
	logger logr.Logger
}

// NewRuleBasedProvider creates a rule-based provider with all built-in rules.
func NewRuleBasedProvider(logger logr.Logger) *RuleBasedProvider {
	p := &RuleBasedProvider{logger: logger}
	p.rules = []Rule{
		{Name: "oom-root-cause", Match: matchOOM, Diagnose: diagnoseOOM},
		{Name: "crashloop-root-cause", Match: matchCrashLoop, Diagnose: diagnoseCrashLoop},
		{Name: "liveness-restarts", Match: matchLivenessRestarts, Diagnose: diagnoseLivenessRestarts},
		{Name: "restart-loop", Match: matchRestartLoop, Diagnose: diagnoseRestartLoop},
		{Name: "readiness-not-ready", Match: matchReadinessNotReady, Diagnose: diagnoseReadinessNotReady},
		{Name: "node-pressure", Match: matchNodePressure, Diagnose: diagnoseNodePressure},
		{Name: "node-down", Match: matchNodeDown, Diagnose: diagnoseNodeDown},
		{Name: "resource-saturation", Match: matchResourceSaturation, Diagnose: diagnoseResourceSaturation},
		{Name: "control-plane", Match: matchControlPlane, Diagnose: diagnoseControlPlane},
		{Name: "image-pull-failure", Match: matchImagePull, Diagnose: diagnoseImagePull},
		{Name: "long-pending-pod", Match: matchPendingPod, Diagnose: diagnosePendingPod},
	}
	return p
}

// Name returns the provider identifier.
func (p *RuleBasedProvider) Name() string {
	return providerNameRuleBased
}

// Diagnose runs all rules against the signals and returns unique diagnoses sorted by confidence.
func (p *RuleBasedProvider) Diagnose(_ context.Context, signals []detection.Signal) ([]Diagnosis, error) {
	if len(signals) == 0 {
		return nil, nil
	}

	diagnoses := make([]Diagnosis, 0, len(p.rules))
	seen := make(map[string]bool)

	for _, rule := range p.rules {
		if !rule.Match(signals) {
			continue
		}
		d := rule.Diagnose(signals)
		if d == nil {
			continue
		}
		// Deduplicate by summary to prevent overlapping rules from producing duplicate diagnoses.
		key := deduplicationKey(d)
		if seen[key] {
			continue
		}
		seen[key] = true

		d.Provider = providerNameRuleBased
		d.DiagnosedAt = time.Now()
		diagnoses = append(diagnoses, *d)
	}

	sort.Slice(diagnoses, func(i, j int) bool {
		return diagnoses[i].Confidence > diagnoses[j].Confidence
	})

	return diagnoses, nil
}

// deduplicationKey creates a key from diagnosis category, suggested action, primary signal and affected resources.
func deduplicationKey(d *Diagnosis) string {
	resources := make([]string, 0, len(d.AffectedResources))
	for _, r := range d.AffectedResources {
		resources = append(resources, fmt.Sprintf("%s/%s/%s", r.Kind, r.Namespace, r.Name))
	}
	sort.Strings(resources)
	// The primary signal is part of the key: two different shapes of a fault on one
	// pod (a restart loop and a crash loop, in different containers) share a category,
	// an action and a pod, and collapsing them hid the milder one's fault entirely.
	primary := ""
	if len(d.Contributing) > 0 {
		primary = string(d.Contributing[0].Signal.Type)
	}
	return fmt.Sprintf("%s:%s:%s:%s", d.Category, d.SuggestedAction, primary, strings.Join(resources, ","))
}

// ============================================================================
// Signal helpers
// ============================================================================

// hasSignalType returns true if any signal matches the given type.
func hasSignalType(signals []detection.Signal, t detection.SignalType) bool {
	for i := range signals {
		if signals[i].Type == t {
			return true
		}
	}
	return false
}

// signalsOfType returns all signals matching the given type.
func signalsOfType(signals []detection.Signal, t detection.SignalType) []detection.Signal {
	var result []detection.Signal
	for i := range signals {
		if signals[i].Type == t {
			result = append(result, signals[i])
		}
	}
	return result
}

// signalsForResource returns signals affecting the same resource (by kind+name+namespace).
func signalsForResource(signals []detection.Signal, ref dorguv1.ResourceReference) []detection.Signal {
	var result []detection.Signal
	for i := range signals {
		s := signals[i]
		if s.Resource.Kind == ref.Kind && s.Resource.Name == ref.Name && s.Resource.Namespace == ref.Namespace {
			result = append(result, s)
		}
	}
	return result
}

// highestSeverity returns the most severe severity from a set of signals.
func highestSeverity(signals []detection.Signal) detection.Severity {
	highest := detection.SeverityInfo
	for _, s := range signals {
		if severityGreater(s.Severity, highest) {
			highest = s.Severity
		}
	}
	return highest
}

var severityRank = map[detection.Severity]int{
	detection.SeverityInfo:     0,
	detection.SeverityWarning:  1,
	detection.SeverityCritical: 2,
}

func severityGreater(a, b detection.Severity) bool {
	return severityRank[a] > severityRank[b]
}

// collectAffectedResources gathers unique ResourceReferences from signals.
func collectAffectedResources(signals []detection.Signal) []dorguv1.ResourceReference {
	seen := make(map[string]bool)
	var resources []dorguv1.ResourceReference
	for _, s := range signals {
		key := fmt.Sprintf("%s/%s/%s", s.Resource.Kind, s.Resource.Namespace, s.Resource.Name)
		if !seen[key] {
			seen[key] = true
			resources = append(resources, s.Resource)
		}
	}
	return resources
}

// firstPersonaRef returns the first non-nil PersonaRef from signals.
func firstPersonaRef(signals []detection.Signal) *dorguv1.PersonaReference {
	for _, s := range signals {
		if s.PersonaRef != nil {
			return s.PersonaRef
		}
	}
	return nil
}

// buildContributing creates ContributingSignal entries from signals with a shared detail.
func buildContributing(signals []detection.Signal, detail string) []ContributingSignal {
	result := make([]ContributingSignal, len(signals))
	for i, s := range signals {
		result[i] = ContributingSignal{Signal: s, Detail: detail}
	}
	return result
}

// concatSignals safely concatenates signal slices without aliasing the input slices.
func concatSignals(slices ...[]detection.Signal) []detection.Signal {
	total := 0
	for _, s := range slices {
		total += len(s)
	}
	result := make([]detection.Signal, 0, total)
	for _, s := range slices {
		result = append(result, s...)
	}
	return result
}

// buildConfidence creates a ConfidenceFactors and calculates confidence.
func buildConfidence(base float64, signals []detection.Signal) float64 {
	return CalculateConfidence(ConfidenceFactors{
		BaseConfidence:    base,
		SignalCount:       len(signals),
		SignalClarity:     AverageClarity(signals),
		TimeWindowSeconds: TimeWindowSeconds(signals),
	})
}

// ============================================================================
// Rule 1: OOM Root Cause
// ============================================================================

func matchOOM(signals []detection.Signal) bool {
	return hasSignalType(signals, detection.SignalOOMKilled)
}

func diagnoseOOM(signals []detection.Signal) *Diagnosis {
	oomSignals := signalsOfType(signals, detection.SignalOOMKilled)
	if len(oomSignals) == 0 {
		return nil
	}

	// Use the first OOM signal as primary; check for correlated memory usage.
	primary := oomSignals[0]
	related := signalsForResource(signals, primary.Resource)
	memUsage := signalsOfType(related, detection.SignalMemoryUsageHigh)

	// A pod that was OOM-killed usually goes on to crash-loop. That is the same
	// event seen twice, not a second finding, so the OOM diagnosis owns the
	// crash-loop signals of every pod it explains (F-9): one OOM, one incident.
	crashLoops := crashLoopsExplainedByOOM(signals)

	var contributing []ContributingSignal
	var summary string
	var base float64

	switch {
	case len(crashLoops) > 0:
		summary = "CrashLoopBackOff caused by OOM kills. Increase memory limits."
		base = 0.90
		contributing = buildContributing(oomSignals, "Container was OOM-killed")
		if len(memUsage) > 0 {
			contributing = append(contributing, buildContributing(memUsage, "Memory usage was near limit")...)
		}
		contributing = append(contributing,
			buildContributing(crashLoops, "Container is crash-looping because it keeps being OOM-killed")...)
	case len(memUsage) > 0:
		val := ""
		if memUsage[0].Value != nil {
			val = fmt.Sprintf(" Memory usage was at %.0f%% of limit.", *memUsage[0].Value)
		}
		summary = "Container memory limit insufficient for workload." + val
		base = 0.85
		contributing = append(
			buildContributing(oomSignals, "Container was OOM-killed"),
			buildContributing(memUsage, "Memory usage was near limit")...,
		)
	default:
		summary = "Container OOM-killed. Memory limit may be insufficient."
		base = 0.70
		contributing = buildContributing(oomSignals, "Container was OOM-killed")
	}

	allRelated := concatSignals(oomSignals, memUsage, crashLoops)

	return &Diagnosis{
		Summary:           summary,
		Confidence:        buildConfidence(base, allRelated),
		Category:          string(detection.CategoryResource),
		Severity:          detection.SeverityCritical,
		PersonaRef:        firstPersonaRef(allRelated),
		AffectedResources: collectAffectedResources(allRelated),
		Contributing:      contributing,
		SuggestedAction:   actionResourceAdjustment,
	}
}

// crashLoopsExplainedByOOM returns the CrashLoopBackOff signals whose pod also
// has an OOMKilled signal: the crash loop is the OOM, and the OOM rule reports it.
func crashLoopsExplainedByOOM(signals []detection.Signal) []detection.Signal {
	var explained []detection.Signal
	for _, crash := range signalsOfType(signals, detection.SignalCrashLoopBackOff) {
		if hasSignalType(signalsForResource(signals, crash.Resource), detection.SignalOOMKilled) {
			explained = append(explained, crash)
		}
	}
	return explained
}

// ============================================================================
// Rule 2: CrashLoop Root Cause
// ============================================================================

func matchCrashLoop(signals []detection.Signal) bool {
	return hasSignalType(signals, detection.SignalCrashLoopBackOff)
}

func diagnoseCrashLoop(signals []detection.Signal) *Diagnosis {
	// A crash loop the pod's own OOM kills explain belongs to the OOM rule: one
	// OOM is one finding (F-9). What is left here is a crash loop with some other
	// cause.
	crashSignals := unexplainedByOOM(signalsOfType(signals, detection.SignalCrashLoopBackOff), signals)
	if len(crashSignals) == 0 {
		return nil
	}

	primary := crashSignals[0]
	related := signalsForResource(signals, primary.Resource)

	var contributing []ContributingSignal
	var summary, action string
	var base float64

	imgRelated := signalsOfType(related, detection.SignalImagePullBackOff)
	probeRelated := signalsOfType(related, detection.SignalProbeFailure)

	switch {
	case len(imgRelated) > 0:
		summary = "CrashLoopBackOff caused by image pull failures."
		action = actionDeploymentFix
		base = 0.90
		contributing = append(
			buildContributing(crashSignals, "Container is crash-looping"),
			buildContributing(imgRelated, "Image pull is failing")...,
		)
	case len(probeRelated) > 0:
		summary = "CrashLoopBackOff likely caused by failing health probes."
		action = actionDeploymentFix
		base = 0.90
		contributing = append(
			buildContributing(crashSignals, "Container is crash-looping"),
			buildContributing(probeRelated, "Health probes are failing")...,
		)
	default:
		summary = "CrashLoopBackOff — check container logs for application errors."
		action = actionInvestigate
		base = 0.50
		contributing = buildContributing(crashSignals, "Container is crash-looping with no correlated signals")
	}

	allSignals := concatSignals(crashSignals, imgRelated, probeRelated)

	return &Diagnosis{
		Summary:           summary,
		Confidence:        buildConfidence(base, allSignals),
		Category:          string(detection.CategoryHealth),
		Severity:          detection.SeverityCritical,
		PersonaRef:        firstPersonaRef(allSignals),
		AffectedResources: collectAffectedResources(allSignals),
		Contributing:      contributing,
		SuggestedAction:   action,
	}
}

// unexplainedByOOM drops the crash-loop signals the OOM rule already reports.
func unexplainedByOOM(crashLoops, all []detection.Signal) []detection.Signal {
	explained := crashLoopsExplainedByOOM(all)
	var rest []detection.Signal
	for i := range crashLoops {
		isExplained := false
		for j := range explained {
			if crashLoops[i].Resource == explained[j].Resource {
				isExplained = true
				break
			}
		}
		if !isExplained {
			rest = append(rest, crashLoops[i])
		}
	}
	return rest
}

// ============================================================================
// Rule 2b: Probe failures (liveness restarts, readiness not Ready)
// ============================================================================
//
// The detector only raises these for a container that is not already in
// CrashLoopBackOff or OOMKilled, so by the time a signal reaches here it is the
// whole story for its container. The rules cannot tell which probe is wrong
// (a bad path, a too-tight timeout, an app that never came up), so the action is
// advice: investigate. That also keeps both inside the gate unchanged: there is
// no patch for the proposer to size and no target path to deduplicate.

func matchLivenessRestarts(signals []detection.Signal) bool {
	return hasSignalType(signals, detection.SignalLivenessRestarts)
}

func diagnoseLivenessRestarts(signals []detection.Signal) *Diagnosis {
	live := signalsOfType(signals, detection.SignalLivenessRestarts)
	if len(live) == 0 {
		return nil
	}
	return &Diagnosis{
		Summary: "The kubelet is killing this container repeatedly for failing its liveness probe, " +
			"without it reaching CrashLoopBackOff. Check the probe's path, port and timeout, and the container's last logs.",
		Confidence:        buildConfidence(0.70, live),
		Category:          string(detection.CategoryHealth),
		Severity:          detection.SeverityWarning,
		PersonaRef:        firstPersonaRef(live),
		AffectedResources: collectAffectedResources(live),
		Contributing:      buildContributing(live, "Container keeps restarting with an error or SIGKILL"),
		SuggestedAction:   actionInvestigate,
	}
}

func matchRestartLoop(signals []detection.Signal) bool {
	return hasSignalType(signals, detection.SignalRestartLoop)
}

func diagnoseRestartLoop(signals []detection.Signal) *Diagnosis {
	loops := signalsOfType(signals, detection.SignalRestartLoop)
	if len(loops) == 0 {
		return nil
	}
	return &Diagnosis{
		Summary: "Container keeps restarting with an error without reaching CrashLoopBackOff, and no kubelet event " +
			"blames a probe, so the cause is not known: it may be an application that crashes quickly or exits on " +
			"its own. Check the container's last logs and exit code.",
		Confidence:        buildConfidence(0.60, loops),
		Category:          string(detection.CategoryHealth),
		Severity:          detection.SeverityWarning,
		PersonaRef:        firstPersonaRef(loops),
		AffectedResources: collectAffectedResources(loops),
		Contributing:      buildContributing(loops, "Container restarted repeatedly with an error or SIGKILL"),
		SuggestedAction:   actionInvestigate,
	}
}

func matchReadinessNotReady(signals []detection.Signal) bool {
	return hasSignalType(signals, detection.SignalReadinessNotReady)
}

func diagnoseReadinessNotReady(signals []detection.Signal) *Diagnosis {
	notReady := signalsOfType(signals, detection.SignalReadinessNotReady)
	if len(notReady) == 0 {
		return nil
	}
	return &Diagnosis{
		Summary: "Container is running but has not been Ready beyond the threshold, so it is not receiving traffic. " +
			"Its readiness probe is failing or the application never started serving. Check the probe and the application's logs.",
		Confidence:        buildConfidence(0.60, notReady),
		Category:          string(detection.CategoryHealth),
		Severity:          detection.SeverityWarning,
		PersonaRef:        firstPersonaRef(notReady),
		AffectedResources: collectAffectedResources(notReady),
		Contributing:      buildContributing(notReady, "Container has been not Ready past the threshold"),
		SuggestedAction:   actionInvestigate,
	}
}

// ============================================================================
// Rule 3: Node Pressure
// ============================================================================

var nodePressureTypes = []detection.SignalType{
	detection.SignalNodeMemoryPressure,
	detection.SignalNodeDiskPressure,
	detection.SignalNodePIDPressure,
}

func matchNodePressure(signals []detection.Signal) bool {
	for _, t := range nodePressureTypes {
		if hasSignalType(signals, t) {
			return true
		}
	}
	return false
}

func diagnoseNodePressure(signals []detection.Signal) *Diagnosis {
	var pressureSignals []detection.Signal
	for _, t := range nodePressureTypes {
		pressureSignals = append(pressureSignals, signalsOfType(signals, t)...)
	}
	if len(pressureSignals) == 0 {
		return nil
	}

	// Check for pod evictions on the same node(s).
	evictionSignals := signalsOfType(signals, detection.SignalPodEvicted)
	// Filter evictions to same nodes as pressure signals.
	nodeNames := make(map[string]bool)
	for _, s := range pressureSignals {
		if s.Resource.Kind == detection.KindNode {
			nodeNames[s.Resource.Name] = true
		}
	}
	var relatedEvictions []detection.Signal
	for _, e := range evictionSignals {
		nodeName := e.Metadata["node"]
		if nodeName == "" {
			nodeName = e.Resource.Name
		}
		if nodeNames[nodeName] {
			relatedEvictions = append(relatedEvictions, e)
		}
	}

	var pressureTypes []string
	seen := make(map[detection.SignalType]bool)
	for _, s := range pressureSignals {
		if !seen[s.Type] {
			seen[s.Type] = true
			pressureTypes = append(pressureTypes, string(s.Type))
		}
	}

	var summary string
	var base float64
	var contributing []ContributingSignal

	if len(relatedEvictions) > 0 {
		summary = fmt.Sprintf("Node under pressure (%s), causing pod evictions.", strings.Join(pressureTypes, ", "))
		base = 0.90
		contributing = append(
			buildContributing(pressureSignals, "Node is under resource pressure"),
			buildContributing(relatedEvictions, "Pods evicted due to node pressure")...,
		)
	} else {
		summary = fmt.Sprintf("Node under pressure: %s.", strings.Join(pressureTypes, ", "))
		base = 0.80
		contributing = buildContributing(pressureSignals, "Node is under resource pressure")
	}

	allSignals := concatSignals(pressureSignals, relatedEvictions)

	return &Diagnosis{
		Summary:           summary,
		Confidence:        buildConfidence(base, allSignals),
		Category:          string(detection.CategoryNode),
		Severity:          highestSeverity(allSignals),
		PersonaRef:        firstPersonaRef(allSignals),
		AffectedResources: collectAffectedResources(allSignals),
		Contributing:      contributing,
		SuggestedAction:   actionResourceAdjustment,
	}
}

// ============================================================================
// Rule 4: Node Down
// ============================================================================

func matchNodeDown(signals []detection.Signal) bool {
	return hasSignalType(signals, detection.SignalNodeNotReady)
}

func diagnoseNodeDown(signals []detection.Signal) *Diagnosis {
	notReadySignals := signalsOfType(signals, detection.SignalNodeNotReady)
	if len(notReadySignals) == 0 {
		return nil
	}

	primary := notReadySignals[0]
	related := signalsForResource(signals, primary.Resource)
	networkDown := signalsOfType(related, detection.SignalNodeNetworkDown)

	var summary, action string
	var contributing []ContributingSignal

	if len(networkDown) > 0 {
		summary = fmt.Sprintf("Node %s unreachable — network connectivity lost.", primary.Resource.Name)
		action = actionInvestigate
		contributing = append(
			buildContributing(notReadySignals, "Node is not ready"),
			buildContributing(networkDown, "Node network is unavailable")...,
		)
	} else {
		summary = fmt.Sprintf("Node %s not ready — may require restart or investigation.", primary.Resource.Name)
		action = "node-restart"
		contributing = buildContributing(notReadySignals, "Node is not ready")
	}

	allSignals := concatSignals(notReadySignals, networkDown)

	return &Diagnosis{
		Summary:           summary,
		Confidence:        buildConfidence(0.85, allSignals),
		Category:          string(detection.CategoryNode),
		Severity:          detection.SeverityCritical,
		PersonaRef:        firstPersonaRef(allSignals),
		AffectedResources: collectAffectedResources(allSignals),
		Contributing:      contributing,
		SuggestedAction:   action,
	}
}

// ============================================================================
// Rule 5: Resource Saturation
// ============================================================================

var saturationCriticalTypes = []detection.SignalType{
	detection.SignalCPUSaturationCritical,
	detection.SignalMemorySaturationCrit,
}

func matchResourceSaturation(signals []detection.Signal) bool {
	for _, t := range saturationCriticalTypes {
		if hasSignalType(signals, t) {
			return true
		}
	}
	return false
}

func diagnoseResourceSaturation(signals []detection.Signal) *Diagnosis {
	var saturationSignals []detection.Signal
	for _, t := range saturationCriticalTypes {
		saturationSignals = append(saturationSignals, signalsOfType(signals, t)...)
	}
	if len(saturationSignals) == 0 {
		return nil
	}

	pendingPods := signalsOfType(signals, detection.SignalPodPendingLong)

	// Build summary with node names and values.
	nodeDetails := make([]string, 0, len(saturationSignals))
	for _, s := range saturationSignals {
		detail := s.Resource.Name
		if s.Value != nil {
			detail = fmt.Sprintf("%s at %.0f%%", s.Resource.Name, *s.Value)
		}
		nodeDetails = append(nodeDetails, detail)
	}

	var summary string
	var base float64
	var contributing []ContributingSignal

	if len(pendingPods) > 0 {
		summary = fmt.Sprintf("Resource saturation (%s). New pods failing to schedule.", strings.Join(nodeDetails, ", "))
		base = 0.90
		contributing = append(
			buildContributing(saturationSignals, "Node resource saturation is critical"),
			buildContributing(pendingPods, "Pods are pending due to resource constraints")...,
		)
	} else {
		summary = fmt.Sprintf("Resource saturation (%s). New pods may fail to schedule.", strings.Join(nodeDetails, ", "))
		base = 0.80
		contributing = buildContributing(saturationSignals, "Node resource saturation is critical")
	}

	allSignals := concatSignals(saturationSignals, pendingPods)

	return &Diagnosis{
		Summary:           summary,
		Confidence:        buildConfidence(base, allSignals),
		Category:          string(detection.CategoryScaling),
		Severity:          detection.SeverityCritical,
		PersonaRef:        firstPersonaRef(allSignals),
		AffectedResources: collectAffectedResources(allSignals),
		Contributing:      contributing,
		SuggestedAction:   "scale-up",
	}
}

// ============================================================================
// Rule 6: Control Plane Issue
// ============================================================================

var controlPlaneTypes = []detection.SignalType{
	detection.SignalAPIServerUnhealthy,
	detection.SignalETCDUnhealthy,
	detection.SignalSchedulerUnhealthy,
	detection.SignalControllerMgrUnhealth,
	detection.SignalComponentUnhealthy,
}

func matchControlPlane(signals []detection.Signal) bool {
	for _, t := range controlPlaneTypes {
		if hasSignalType(signals, t) {
			return true
		}
	}
	return false
}

func diagnoseControlPlane(signals []detection.Signal) *Diagnosis {
	var cpSignals []detection.Signal
	for _, t := range controlPlaneTypes {
		cpSignals = append(cpSignals, signalsOfType(signals, t)...)
	}
	if len(cpSignals) == 0 {
		return nil
	}

	var components []string
	seen := make(map[string]bool)
	for _, s := range cpSignals {
		name := string(s.Type)
		if !seen[name] {
			seen[name] = true
			components = append(components, name)
		}
	}

	summary := fmt.Sprintf("Control plane degraded: %s.", strings.Join(components, ", "))

	return &Diagnosis{
		Summary:           summary,
		Confidence:        buildConfidence(0.95, cpSignals),
		Category:          string(detection.CategoryControlPlane),
		Severity:          detection.SeverityCritical,
		PersonaRef:        firstPersonaRef(cpSignals),
		AffectedResources: collectAffectedResources(cpSignals),
		Contributing:      buildContributing(cpSignals, "Control plane component is unhealthy"),
		SuggestedAction:   actionInvestigate,
	}
}

// ============================================================================
// Rule 7: Image Pull Failure
// ============================================================================

func matchImagePull(signals []detection.Signal) bool {
	return hasSignalType(signals, detection.SignalImagePullBackOff)
}

func diagnoseImagePull(signals []detection.Signal) *Diagnosis {
	imgSignals := signalsOfType(signals, detection.SignalImagePullBackOff)
	if len(imgSignals) == 0 {
		return nil
	}

	// Filter out image pull signals already covered by the CrashLoop rule (same pod).
	crashSignals := signalsOfType(signals, detection.SignalCrashLoopBackOff)
	if len(crashSignals) > 0 {
		crashPods := make(map[string]bool)
		for _, c := range crashSignals {
			crashPods[c.Resource.Namespace+"/"+c.Resource.Name] = true
		}
		var filtered []detection.Signal
		for _, img := range imgSignals {
			if !crashPods[img.Resource.Namespace+"/"+img.Resource.Name] {
				filtered = append(filtered, img)
			}
		}
		imgSignals = filtered
	}
	if len(imgSignals) == 0 {
		return nil
	}

	return &Diagnosis{
		Summary:           "Image pull failing for container. Check image name, tag, and registry credentials.",
		Confidence:        buildConfidence(0.80, imgSignals),
		Category:          string(detection.CategoryDeployment),
		Severity:          detection.SeverityCritical,
		PersonaRef:        firstPersonaRef(imgSignals),
		AffectedResources: collectAffectedResources(imgSignals),
		Contributing:      buildContributing(imgSignals, "Image pull is failing"),
		SuggestedAction:   actionDeploymentFix,
	}
}

// ============================================================================
// Rule 8: Long Pending Pod
// ============================================================================

func matchPendingPod(signals []detection.Signal) bool {
	return hasSignalType(signals, detection.SignalPodPendingLong)
}

func diagnosePendingPod(signals []detection.Signal) *Diagnosis {
	pendingSignals := signalsOfType(signals, detection.SignalPodPendingLong)
	if len(pendingSignals) == 0 {
		return nil
	}

	// Skip if already handled by resource saturation rule.
	hasSaturation := false
	for _, t := range saturationCriticalTypes {
		if hasSignalType(signals, t) {
			hasSaturation = true
			break
		}
	}

	var summary string
	var base float64
	var contributing []ContributingSignal

	if hasSaturation {
		// Saturation rule already covers this; only produce standalone pending diagnosis
		// if there are pending pods NOT on saturated nodes.
		return nil
	}

	summary = "Pod pending for extended period. May be waiting for resources, node affinity, or tolerations."
	base = 0.50
	contributing = buildContributing(pendingSignals, "Pod has been pending beyond threshold")

	return &Diagnosis{
		Summary:           summary,
		Confidence:        buildConfidence(base, pendingSignals),
		Category:          string(detection.CategoryScaling),
		Severity:          detection.SeverityWarning,
		PersonaRef:        firstPersonaRef(pendingSignals),
		AffectedResources: collectAffectedResources(pendingSignals),
		Contributing:      contributing,
		SuggestedAction:   actionResourceAdjustment,
	}
}
