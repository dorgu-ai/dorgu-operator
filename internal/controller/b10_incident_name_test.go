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
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dorgu-ai/dorgu-operator/internal/harness/gate"
)

// B10 (d), D1.1 finding 8: a recurrence or an escalation keeps the NAME of the incident it
// continues, so an OOM incident on podinfo is called `...crashloopbackoff...-r3`. The name
// is not renamed, because it is the lineage: the gate's dedup, the rejection cooldown and
// every RemediationAction's incidentRef are keyed on the name without its -rN suffix, an
// escalating incident is the same object (an object cannot be renamed), and a recurrence
// named after its own signal would be a different lineage from the fault it recurs, so a
// human's rejection would stop holding. What the incident currently shows is its signal,
// carried by the `dorgu.io/signal` label and the Signal column of `kubectl get`.

func TestIncidentName_ARecurrenceKeepsItsLineageAndShowsItsOwnCurrentSignal(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setCrashLoop()
	e.cycle()
	first := e.onlyOpen()
	require.Contains(t, first.Name, "crashloopbackoff")
	e.resolve(first.Name)

	e.setOOM()
	e.cycle()

	recurrence := e.onlyOpen()
	assert.True(t, gate.SameIncidentLineage(first.Name, recurrence.Name), "the lineage is kept")
	assert.Contains(t, recurrence.Name, "crashloopbackoff", "the name is the lineage's, not the current signal's")
	assert.Equal(t, "OOMKilled", recurrence.Spec.Detection.Signal)
	assert.Equal(t, "OOMKilled", recurrence.Labels[LabelSignal], "the label says what it is now")
}

func TestIncidentName_AnEscalationShowsItsCurrentSignalOnTheSameObject(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setReadiness()
	e.cycle()
	first := e.onlyOpen()
	require.Contains(t, first.Name, "readinessnotready")

	e.setOOM()
	e.cycle()

	escalated := e.onlyOpen()
	assert.Equal(t, first.Name, escalated.Name, "an incident that escalates is the same object, so it keeps its name")
	assert.Equal(t, "OOMKilled", escalated.Spec.Detection.Signal)
	assert.Equal(t, "OOMKilled", escalated.Labels[LabelSignal])
}

// The Signal printer column is what `kubectl get incidentmemories` shows beside the name.
func TestIncidentName_TheCRDShowsTheCurrentSignalColumn(t *testing.T) {
	raw, err := os.ReadFile("../../charts/dorgu-operator/crds/dorgu.io_incidentmemories.yaml")
	require.NoError(t, err)
	text := string(raw)
	assert.True(t, strings.Contains(text, "jsonPath: .spec.detection.signal"), "the Signal column reads the current signal")
	assert.Contains(t, text, "name: Signal")
}
