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

package gate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/safetext"
)

// GuardrailRollbackWouldBePartial: a rollback is whole or it is refused. The gate may
// clamp a field of a restore, strip one or drop a step, and a ticket for what is left
// would put back some of what Dorgu changed and leave the rest. The rule is checked at
// the request (intake), at the approval re-check and at the apply-time re-run, and all
// three use PartialRollback. Its condition reason is RollbackWouldBePartial.
const GuardrailRollbackWouldBePartial = "rollback-would-be-partial"

// PartialRollback says why the decision is not the whole rollback that was asked for, or
// returns "" when it is whole. want is the plan the rollback derived (at intake) or the
// plan the action carries (at approval and apply); restoring names the fields it puts
// back, for the message, and is read from want when empty.
//
// Whole means a plain pass with every step of want coming through unchanged: the same
// type, the same auto-executable flag and the same patch as a JSON document. Nothing
// here relaxes a cap. They stay in force on a rollback, and are what catches a change
// made between the request and the approval.
//
// The comparison is the safe way round: a patch that is absent or not JSON on either
// side counts as different, so a false alarm refuses and never applies half.
func PartialRollback(dec *Decision, want []dorguv1.RemediationStep, restoring []string) string {
	what := partialWhat(dec, want)
	if what == "" {
		return ""
	}
	if len(restoring) == 0 {
		restoring = restoredBy(want)
	}
	return partialMessage(dec, restoring, what)
}

// partialWhat is the short description of how the decision differs from want.
func partialWhat(dec *Decision, want []dorguv1.RemediationStep) string {
	switch {
	case dec.Result != ResultPass:
		name := string(dec.Result)
		if name == "" {
			name = "unknown"
		}
		return fmt.Sprintf("its result was %s, not a pass", name)
	case len(dec.Steps) != len(want):
		return fmt.Sprintf("it kept %d of the %d steps", len(dec.Steps), len(want))
	}
	for i := range want {
		w, got := &want[i], &dec.Steps[i]
		switch {
		case got.Type != w.Type || got.AutoExecutable != w.AutoExecutable:
			return fmt.Sprintf("it changed what step %d does", w.Order)
		case !samePatchDocument(w.Patch, got.Patch):
			return fmt.Sprintf("it changed the values step %d restores", w.Order)
		}
	}
	return ""
}

// restoredBy names the persona-spec paths the plan's patches write, sorted.
func restoredBy(steps []dorguv1.RemediationStep) []string {
	var paths []string
	for i := range steps {
		if steps[i].Patch == nil {
			continue
		}
		for path := range PatchLeafValues(steps[i].Patch.Raw) {
			if !slices.Contains(paths, path) {
				paths = append(paths, path)
			}
		}
	}
	slices.Sort(paths)
	return paths
}

// samePatchDocument compares two patches as JSON documents. A patch that is absent on
// either side, or is not JSON, is not the same.
func samePatchDocument(a, b *apiextensionsv1.JSON) bool {
	if a == nil || b == nil {
		return false
	}
	var left, right any
	if json.Unmarshal(a.Raw, &left) != nil || json.Unmarshal(b.Raw, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

// partialMessage is what the refusal says: what the gate did, and the guardrail that did
// it when the gate named one.
func partialMessage(dec *Decision, restoring []string, what string) string {
	names := strings.Join(restoring, ", ")
	if names == "" {
		names = "the recorded values"
	}
	msg := fmt.Sprintf("the gate would not clear the whole restore of %s as derived (%s), and Dorgu restores everything it recorded or nothing: "+
		"the rollback is refused, not partially applied, and nothing was applied", names, what)
	for _, v := range dec.Verdicts {
		if v.Verdict != dorguv1.GateVerdictPass && v.Reason != "" {
			return fmt.Sprintf("%s. The gate said (%s): %s", msg, v.Guardrail, safetext.Clean(v.Reason, true))
		}
	}
	return msg
}

// enforceWholeRollback is the last thing a re-run of the gate does for a rollback: it
// turns a decision that would let a narrowed restore through into a refusal. The
// proposal phase is left to intake, which rejects with its own reason and makes no
// action; everything after that (the approval re-check, and the apply-time re-run that
// re-derives the ticket) comes through here, so no later run can issue or honour a
// ticket for part of a restore.
func enforceWholeRollback(cand *Candidate, phase Phase, d *Decision) {
	if phase != PhaseApprovalRecheck || cand.RollbackOf == "" || d.Result == ResultRefuse {
		return
	}
	if msg := PartialRollback(d, cand.Steps, nil); msg != "" {
		d.refuse(StageGuardrails, GuardrailRollbackWouldBePartial, msg, nil).From(dorguv1.CeilingSourceDefault)
	}
}
