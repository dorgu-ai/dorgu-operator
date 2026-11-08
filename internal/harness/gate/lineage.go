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
	// which truncation must never cut into.
	hashSegmentLength = 13
)

var generationSuffix = regexp.MustCompile(`-r([0-9]+)$`)

// IncidentLineage returns the name shared by every record of one fault: the
// incident name without its recurrence suffix. A base name always ends in a hash
// of hex digits, so a trailing "-r<N>" can only be a recurrence suffix.
func IncidentLineage(name string) string {
	return generationSuffix.ReplaceAllString(name, "")
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
// name is base, shortening the readable prefix, never the hash, to stay within
// the Kubernetes name limit. Generation 0 is base itself.
func RecurrenceName(base string, generation int) string {
	base = IncidentLineage(base)
	if generation <= 0 {
		return base
	}
	suffix := fmt.Sprintf("-r%d", generation)
	if len(base)+len(suffix) <= maxIncidentNameLength || len(base) <= hashSegmentLength {
		return base + suffix
	}
	keep := maxIncidentNameLength - len(suffix) - hashSegmentLength
	return base[:keep] + base[len(base)-hashSegmentLength:] + suffix
}
