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

package detection

import "testing"

// IsClusterScopedKind is the one list of kinds that legitimately have no
// namespace. Everything else with an empty namespace is "not determined".
func TestIsClusterScopedKind(t *testing.T) {
	for kind, want := range map[string]bool{
		KindNode:            true,
		KindComponentStatus: true,
		"Pod":               false,
		"Deployment":        false,
		"":                  false,
		"node":              false, // kinds are case-sensitive
	} {
		if got := IsClusterScopedKind(kind); got != want {
			t.Errorf("IsClusterScopedKind(%q) = %v, want %v", kind, got, want)
		}
	}
}
