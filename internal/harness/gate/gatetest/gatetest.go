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

// Package gatetest is for tests only. It computes, for a RemediationAction held in
// memory, the specHash a fake client would serve for it. Production code must
// never use it: the operator hashes what the API server serves (Gate.ServedSpecHash).
package gatetest

import (
	"encoding/json"
	"fmt"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// ServedSpecHash is the specHash of ra's spec as a fake client serves it: its JSON
// encoding, decoded as a generic document, hashed as P2-D14 says. A real API server
// can serve a different document for a client-written spec (that is what the
// envtest suite in test/crdratchet covers); a fake serves exactly the typed JSON.
// It panics on a spec that cannot be encoded, which no fixture has.
func ServedSpecHash(ra *dorguv1.RemediationAction) string {
	raw, err := json.Marshal(ra.Spec)
	if err != nil {
		panic(fmt.Sprintf("gatetest: encoding the spec: %v", err))
	}
	hash, err := dorguv1.SpecHashFromJSON(raw)
	if err != nil {
		panic(fmt.Sprintf("gatetest: hashing the spec: %v", err))
	}
	return hash
}
