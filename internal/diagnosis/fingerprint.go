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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/dorgu-ai/dorgu-operator/internal/detection"
)

// Fingerprint identifies the situation a diagnosis describes, for deciding
// whether a model has already been asked about it. Two diagnoses of the same
// application with the same fingerprint are the same question.
//
// It is built only from what the rules decided and what the detectors saw, never
// from model text: the category, the rule-owned suggested action, the severity,
// the persona, and for each contributing signal its type, severity and resource.
// A pod resource is named by its workload, because pod names carry a per-replica
// suffix and a restart would otherwise make every cycle look new.
//
// It deliberately leaves out everything that moves while the situation does not:
// signal messages (restart counts tick up), values (a CPU percentage jitters),
// detection times and confidence. An incident's occurrence count is left out for
// the same reason: it rises every cycle.
func Fingerprint(d Diagnosis) string {
	h := sha256.New()
	write := func(format string, args ...any) { _, _ = fmt.Fprintf(h, format, args...) }

	write("category=%s\naction=%s\nseverity=%s\n", d.Category, d.SuggestedAction, d.Severity)
	if d.PersonaRef != nil {
		write("persona=%s/%s/%s\n", d.PersonaRef.Kind, d.PersonaRef.Namespace, d.PersonaRef.Name)
	}

	seen := make(map[string]struct{}, len(d.Contributing))
	lines := make([]string, 0, len(d.Contributing))
	for i := range d.Contributing {
		sig := d.Contributing[i].Signal
		line := strings.Join([]string{
			string(sig.Type), string(sig.Severity),
			sig.Resource.Kind, sig.Resource.Namespace, detection.WorkloadName(&sig),
		}, "|")
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	for _, line := range lines {
		write("signal=%s\n", line)
	}

	return hex.EncodeToString(h.Sum(nil))
}
