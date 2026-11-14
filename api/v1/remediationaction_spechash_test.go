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

func mustHash(t *testing.T, spec RemediationActionSpec) string {
	t.Helper()
	h, err := spec.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return h
}

func TestSpecHash_IsLowercaseHexSHA256(t *testing.T) {
	h := mustHash(t, hashTestSpec())
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) {
		t.Fatalf("hash %q is not 64 lowercase hex characters", h)
	}
}

func TestSpecHash_IsTheSHA256OfTheDocumentedCanonicalEncoding(t *testing.T) {
	spec := hashTestSpec()
	canonical, err := spec.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	if got, want := mustHash(t, spec), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("hash = %s, want sha256(CanonicalJSON) = %s", got, want)
	}
}

func TestSpecHash_CanonicalJSONHasSortedKeysAndNoWhitespace(t *testing.T) {
	spec := hashTestSpec()
	spec.Steps[0].Patch = &apiextensionsv1.JSON{Raw: []byte("{ \"spec\": {\"resources\": {\"requests\": {\"memory\": \"1Gi\"},\n \"limits\": {\"memory\": \"512Mi\"}}} }")}
	canonical, err := spec.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `"patch":{"spec":{"resources":{"limits":{"memory":"512Mi"},"requests":{"memory":"1Gi"}}}}`
	if !strings.Contains(string(canonical), want) {
		t.Fatalf("canonical JSON %s does not contain %s", canonical, want)
	}
	for _, b := range canonical {
		if b == ' ' || b == '\n' || b == '\t' {
			t.Fatalf("canonical JSON contains whitespace: %s", canonical)
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
	for i := 0; i < 20; i++ {
		if got := mustHash(t, hashTestSpec()); got != first {
			t.Fatalf("run %d: %s != %s", i, got, first)
		}
	}
}

func TestSpecHash_APatchThatIsNotJSONIsAnError(t *testing.T) {
	spec := hashTestSpec()
	spec.Steps[0].Patch = &apiextensionsv1.JSON{Raw: []byte(`{not json`)}
	if _, err := spec.Hash(); err == nil {
		t.Fatal("a spec with an unencodable patch must not hash")
	}
}

func TestSpecHash_DoesNotEscapeHTMLCharacters(t *testing.T) {
	spec := hashTestSpec()
	spec.Explanation = "a < b && c > d"
	canonical, err := spec.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"explanation":"a < b && c > d"`) {
		t.Fatalf("canonical JSON escaped HTML characters: %s", canonical)
	}
}
