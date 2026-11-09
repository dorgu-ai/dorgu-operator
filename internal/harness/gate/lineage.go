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
	"fmt"
	"regexp"
	"strconv"
)

// An incident's name is "im-<namespace>-<persona>-<signal>-<hash>", a stable hash
// of the fault. The first occurrence has exactly that name. When the same fault
// comes back after the incident resolved, it is recorded as a NEW incident whose
// name adds "-r<N>" (N = 1 for the first recurrence): same fault, new record, its
// own firstSeen, phase, time-to-detect and proposal. Every record of one fault
// shares a lineage, which is the name without the suffix.
//
// The lineage is what lets a cooldown that is keyed on "this incident" keep
// holding across a recurrence: a human's rejection of the fix for incident A is
// still a rejection of the same fix when A's recurrence A-r1 asks for it.

const (
	// maxIncidentNameLength is the Kubernetes object name limit.
	maxIncidentNameLength = 253

	// hashSegmentLength is the "-" and 12 hex digits that end every incident name,
	// which shortening must never cut into.
	hashSegmentLength = 13

	// maxLineageLength is the longest lineage key. A recurrence name is the lineage
	// plus "-r<N>", and 13 characters is room for a ten-digit N, so a recurrence
	// name always fits. A first-occurrence name longer than this (the generator can
	// emit 252 characters) is shortened, in the middle and never in the hash, to
	// form its lineage, so that the first occurrence and every recurrence of it
	// still share one.
	maxLineageLength = maxIncidentNameLength - 13
)

// AnnotationAnswersSignal is the annotation a RemediationAction carries naming the
// signal of the diagnosis it answers, so that a rejection can later be read as a
// no to THAT shape of a fault and not to every shape (see rejectionReason).
const AnnotationAnswersSignal = "dorgu.io/answers-signal"

var generationSuffix = regexp.MustCompile(`-r([0-9]+)$`)

// withoutGeneration removes a trailing recurrence suffix. A base name always ends
// in a hash of hex digits, so a trailing "-r<N>" can only be a recurrence suffix.
func withoutGeneration(name string) string {
	return generationSuffix.ReplaceAllString(name, "")
}

// IncidentLineage returns the key shared by every record of one fault: the
// incident name without its recurrence suffix, shortened to maxLineageLength when
// it is longer. For every name shorter than that it is just the first-occurrence
// name.
func IncidentLineage(name string) string {
	base := withoutGeneration(name)
	if len(base) <= maxLineageLength || len(base) <= hashSegmentLength {
		return base
	}
	keep := maxLineageLength - hashSegmentLength
	return base[:keep] + base[len(base)-hashSegmentLength:]
}

// SameIncidentLineage reports whether two incident names record the same fault.
func SameIncidentLineage(a, b string) bool {
	return a != "" && IncidentLineage(a) == IncidentLineage(b)
}

// IncidentGeneration returns 0 for a first-occurrence name and N for the Nth
// recurrence.
func IncidentGeneration(name string) int {
	m := generationSuffix.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// RecurrenceName names generation N (N >= 1) of the fault whose first-occurrence
// name is base: the lineage plus "-r<N>", always within the Kubernetes name
// limit. Generation 0 is the first-occurrence name itself.
func RecurrenceName(base string, generation int) string {
	if generation <= 0 {
		return withoutGeneration(base)
	}
	return IncidentLineage(base) + fmt.Sprintf("-r%d", generation)
}
