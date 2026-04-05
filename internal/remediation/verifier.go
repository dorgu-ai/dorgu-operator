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

package remediation

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// VerificationResult represents the outcome of post-apply health verification.
type VerificationResult string

const (
	VerificationHealthy  VerificationResult = "Healthy"
	VerificationDegraded VerificationResult = "Degraded"
	VerificationUnknown  VerificationResult = "Unknown"
)

// Verifier performs post-apply health verification by re-running detection
// and checking if the original incident signal has cleared.
type Verifier struct {
	detection *detection.Engine
	client    client.Client
	logger    logr.Logger
}

// NewVerifier creates a new Verifier.
func NewVerifier(det *detection.Engine, c client.Client, logger logr.Logger) *Verifier {
	return &Verifier{
		detection: det,
		client:    c,
		logger:    logger.WithName("verifier"),
	}
}

// Verify checks whether the remediation was effective by re-running detection
// and comparing current signals against the original incident.
func (v *Verifier) Verify(ctx context.Context, action *dorguv1.RemediationAction) (VerificationResult, error) {
	// 1. Collect current signals from the detection engine.
	signals, err := v.detection.CollectAll(ctx)
	if err != nil {
		v.logger.Error(err, "detection engine failed during verification")
		return VerificationUnknown, fmt.Errorf("collecting signals: %w", err)
	}

	// 2. Look up the original incident to get the signal we're checking for.
	incident, err := v.getIncident(ctx, action)
	if err != nil {
		v.logger.Error(err, "failed to get incident for verification")
		return VerificationUnknown, fmt.Errorf("getting incident: %w", err)
	}

	originalSignal := incident.Spec.Detection.Signal
	personaName := action.Spec.PersonaRef.Name
	personaNamespace := action.Spec.PersonaRef.Namespace

	// 3. Check if the original signal is still present for this persona.
	if v.hasSignalForPersona(signals, originalSignal, personaName, personaNamespace) {
		v.logger.Info("original signal still present after remediation",
			"signal", originalSignal,
			"persona", fmt.Sprintf("%s/%s", personaNamespace, personaName),
		)
		return VerificationDegraded, nil
	}

	// 4. Check for new critical signals affecting the same persona.
	if v.hasNewCriticalSignals(signals, personaName, personaNamespace) {
		v.logger.Info("new critical signals detected after remediation",
			"persona", fmt.Sprintf("%s/%s", personaNamespace, personaName),
		)
		return VerificationDegraded, nil
	}

	v.logger.Info("verification passed: original signal cleared",
		"signal", originalSignal,
		"persona", fmt.Sprintf("%s/%s", personaNamespace, personaName),
	)
	return VerificationHealthy, nil
}

// getIncident fetches the IncidentMemory referenced by the action.
func (v *Verifier) getIncident(ctx context.Context, action *dorguv1.RemediationAction) (*dorguv1.IncidentMemory, error) {
	namespace := action.Spec.IncidentRef.Namespace
	if namespace == "" {
		namespace = action.Namespace
	}

	var incident dorguv1.IncidentMemory
	key := client.ObjectKey{
		Name:      action.Spec.IncidentRef.Name,
		Namespace: namespace,
	}
	if err := v.client.Get(ctx, key, &incident); err != nil {
		return nil, fmt.Errorf("getting IncidentMemory %s/%s: %w", namespace, action.Spec.IncidentRef.Name, err)
	}

	return &incident, nil
}

// hasSignalForPersona checks if a specific signal type is present for the given persona.
func (v *Verifier) hasSignalForPersona(signals []detection.Signal, signalType string, personaName, personaNamespace string) bool {
	for i := range signals {
		if signals[i].PersonaRef == nil {
			continue
		}
		if string(signals[i].Type) == signalType &&
			signals[i].PersonaRef.Name == personaName &&
			signals[i].PersonaRef.Namespace == personaNamespace {
			return true
		}
	}
	return false
}

// hasNewCriticalSignals checks for any new critical-severity signals affecting the persona.
func (v *Verifier) hasNewCriticalSignals(signals []detection.Signal, personaName, personaNamespace string) bool {
	for i := range signals {
		if signals[i].PersonaRef == nil {
			continue
		}
		if signals[i].Severity == detection.SeverityCritical &&
			signals[i].PersonaRef.Name == personaName &&
			signals[i].PersonaRef.Namespace == personaNamespace {
			return true
		}
	}
	return false
}
