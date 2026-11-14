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

package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func hashTestSpec() RemediationActionSpec {
	return RemediationActionSpec{
		PersonaRef:  PersonaReference{Kind: "ApplicationPersona", Name: "web", Namespace: "shop"},
		IncidentRef: IncidentReference{Name: "inc-1", Namespace: "shop"},
		Explanation: "raise the memory limit",
		Steps: []RemediationStep{{
			Order: 1, ID: "s1", Type: StepTypePersonaUpdate, AutoExecutable: true,
			Patch: &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi"}}}}`)},
		}},
	}
}

// typedSpecJSON is the JSON a client-go typed write of the spec sends. The
// operator no longer hashes it (it hashes what the API server serves, P2-D14);
// these tests use it only as a stand-in for a served document.
func typedSpecJSON(t *testing.T, spec RemediationActionSpec) []byte {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("encoding the spec: %v", err)
	}
	return raw
}

func mustHash(t *testing.T, spec RemediationActionSpec) string {
	t.Helper()
	h, err := SpecHashFromJSON(typedSpecJSON(t, spec))
	if err != nil {
		t.Fatalf("SpecHashFromJSON: %v", err)
	}
	return h
}

func mustCanonical(t *testing.T, spec RemediationActionSpec) []byte {
	t.Helper()
	canonical, err := CanonicalSpecJSON(typedSpecJSON(t, spec))
	if err != nil {
		t.Fatalf("CanonicalSpecJSON: %v", err)
	}
	return canonical
}

func TestSpecHash_IsLowercaseHexSHA256(t *testing.T) {
	h := mustHash(t, hashTestSpec())
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) {
		t.Fatalf("hash %q is not 64 lowercase hex characters", h)
	}
}

func TestSpecHash_IsTheSHA256OfTheDocumentedCanonicalEncoding(t *testing.T) {
	spec := hashTestSpec()
	canonical := mustCanonical(t, spec)
	sum := sha256.Sum256(canonical)
	if got, want := mustHash(t, spec), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("hash = %s, want sha256(CanonicalJSON) = %s", got, want)
	}
}

func TestSpecHash_CanonicalJSONHasSortedKeysAndNoWhitespace(t *testing.T) {
	spec := hashTestSpec()
	spec.Steps[0].Patch = &apiextensionsv1.JSON{Raw: []byte("{ \"spec\": {\"resources\": {\"requests\": {\"memory\": \"1Gi\"},\n \"limits\": {\"memory\": \"512Mi\"}}} }")}
	canonical := mustCanonical(t, spec)
	want := `"patch":{"spec":{"resources":{"limits":{"memory":"512Mi"},"requests":{"memory":"1Gi"}}}}`
	if !strings.Contains(string(canonical), want) {
		t.Fatalf("canonical JSON %s does not contain %s", canonical, want)
	}
	// Whitespace inside a string value is data; between tokens it must not exist.
	for _, between := range []string{`": `, `, "`, "{ ", " }", "[ ", " ]", "\n", "\t"} {
		if strings.Contains(string(canonical), between) {
			t.Fatalf("canonical JSON has whitespace between tokens (%q): %s", between, canonical)
		}
	}
}

func TestSpecHash_IgnoresKeyOrderAndSpacingInAPatch(t *testing.T) {
	a := hashTestSpec()
	a.Steps[0].Patch = &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"resources":{"limits":{"memory":"512Mi","cpu":"1"},"requests":{"cpu":"1"}}}}`)}
	b := hashTestSpec()
	b.Steps[0].Patch = &apiextensionsv1.JSON{Raw: []byte(`{ "spec": { "resources": { "requests": {"cpu":"1"}, "limits": {"cpu":"1", "memory":"512Mi"} } } }`)}

	if mustHash(t, a) != mustHash(t, b) {
		t.Fatal("two encodings of the same patch hash differently")
	}
}

func TestSpecHash_ChangesWhenAnythingInTheSpecChanges(t *testing.T) {
	base := mustHash(t, hashTestSpec())
	mutations := map[string]func(*RemediationActionSpec){
		"patch value": func(s *RemediationActionSpec) {
			s.Steps[0].Patch.Raw = []byte(`{"spec":{"resources":{"limits":{"memory":"1Gi"}}}}`)
		},
		"explanation":  func(s *RemediationActionSpec) { s.Explanation = "something else" },
		"persona":      func(s *RemediationActionSpec) { s.PersonaRef.Name = "api" },
		"step command": func(s *RemediationActionSpec) { s.Steps[0].Command = "kubectl get pods" },
		"extra step": func(s *RemediationActionSpec) {
			s.Steps = append(s.Steps, RemediationStep{Order: 2, ID: "s2", Type: StepTypeManual})
		},
		"auto-executable": func(s *RemediationActionSpec) { s.Steps[0].AutoExecutable = false },
		"legacy action":   func(s *RemediationActionSpec) { s.Action.Type = ActionTypeNotification },
		"trust level":     func(s *RemediationActionSpec) { s.TrustLevel = 3 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			spec := hashTestSpec()
			mutate(&spec)
			if mustHash(t, spec) == base {
				t.Fatalf("changing %s did not change the hash", name)
			}
		})
	}
}

func TestSpecHash_IsDeterministic(t *testing.T) {
	first := mustHash(t, hashTestSpec())
	for i := range 20 {
		if got := mustHash(t, hashTestSpec()); got != first {
			t.Fatalf("run %d: %s != %s", i, got, first)
		}
	}
}

func TestSpecHash_APatchThatIsNotJSONIsAnError(t *testing.T) {
	spec := hashTestSpec()
	spec.Steps[0].Patch = &apiextensionsv1.JSON{Raw: []byte(`{not json`)}
	if _, err := json.Marshal(spec); err == nil {
		t.Fatal("a spec with an unencodable patch must not encode, so it cannot hash")
	}
	if _, err := SpecHashFromJSON([]byte(`{"patch":{not json}`)); err == nil {
		t.Fatal("a document that is not JSON must not hash")
	}
}

func TestSpecHash_DoesNotEscapeHTMLCharacters(t *testing.T) {
	spec := hashTestSpec()
	spec.Explanation = "a < b && c > d"
	canonical := mustCanonical(t, spec)
	if !strings.Contains(string(canonical), `"explanation":"a < b && c > d"`) {
		t.Fatalf("canonical JSON escaped HTML characters: %s", canonical)
	}
}

// Why the operator hashes the served document and never a typed struct (P2-D14,
// C3 re-review N-1): a spec decoded into the Go types and encoded again is another
// document for each of these inputs, so a client hashing the served spec would not
// match a typed hash.
func TestSpecHash_ATypedRoundTripIsNotTheServedDocument(t *testing.T) {
	served := map[string]string{
		"empty steps":      `{"steps":[]}`,
		"empty safety":     `{"steps":[{"order":1,"id":"s","type":"manual","description":"d","autoExecutable":false,"trustLevel":0,"safety":[]}]}`,
		"duration spelled": `{"rollback":{"enabled":true,"maxRetries":1,"healthCheckAfter":"5m"}}`,
		"fractional time":  `{"workloadRef":{"kind":"Deployment","name":"w","namespace":"n","managedBy":"unknown","observedAt":"2026-11-14T10:00:00.123Z"}}`,
		"offset time":      `{"workloadRef":{"kind":"Deployment","name":"w","namespace":"n","managedBy":"unknown","observedAt":"2026-11-14T15:30:00+05:30"}}`,
	}
	for name, doc := range served {
		t.Run(name, func(t *testing.T) {
			var typed RemediationActionSpec
			if err := json.Unmarshal([]byte(doc), &typed); err != nil {
				t.Fatal(err)
			}
			again, err := json.Marshal(typed)
			if err != nil {
				t.Fatal(err)
			}
			fromServed, err := SpecHashFromJSON([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			fromTyped, err := SpecHashFromJSON(again)
			if err != nil {
				t.Fatal(err)
			}
			if fromServed == fromTyped {
				t.Fatalf("expected the typed round trip of %s to differ from the served document", doc)
			}
		})
	}
}
