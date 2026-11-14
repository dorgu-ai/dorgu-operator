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
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

type specHashVector struct {
	Name      string          `json:"name"`
	Note      string          `json:"note"`
	Spec      json.RawMessage `json:"spec"`
	Canonical string          `json:"canonical"`
	SHA256    string          `json:"sha256"`
}

func loadSpecHashVectors(t *testing.T) []specHashVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/spechash_vectors.json")
	if err != nil {
		t.Fatalf("reading the golden vectors: %v", err)
	}
	var file struct {
		Vectors []specHashVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decoding the golden vectors: %v", err)
	}
	if len(file.Vectors) < 8 {
		t.Fatalf("only %d golden vectors: the file must cover unicode, escapes, key order, numbers, patches and provenance", len(file.Vectors))
	}
	return file.Vectors
}

// The vectors are the cross-language contract (P2-D14): a CLI, a dashboard or a
// client in another language computes the same hash from the same served spec.
func TestSpecHashVectors_CanonicalTextAndHash(t *testing.T) {
	for _, v := range loadSpecHashVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			canonical, err := CanonicalSpecJSON(v.Spec)
			if err != nil {
				t.Fatalf("CanonicalSpecJSON: %v", err)
			}
			if string(canonical) != v.Canonical {
				t.Fatalf("canonical text\n got %q\nwant %q", canonical, v.Canonical)
			}
			hash, err := SpecHashFromJSON(v.Spec)
			if err != nil {
				t.Fatalf("SpecHashFromJSON: %v", err)
			}
			if hash != v.SHA256 {
				t.Fatalf("hash = %s, want %s", hash, v.SHA256)
			}
		})
	}
}

func TestSpecHashVectors_MapHelperAgreesWithRawJSON(t *testing.T) {
	for _, v := range loadSpecHashVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			var asFloats map[string]any
			if err := json.Unmarshal(v.Spec, &asFloats); err != nil {
				t.Fatal(err)
			}
			var asNumbers map[string]any
			dec := json.NewDecoder(bytes.NewReader(v.Spec))
			dec.UseNumber()
			if err := dec.Decode(&asNumbers); err != nil {
				t.Fatal(err)
			}
			for label, doc := range map[string]map[string]any{"float64": asFloats, "json.Number": asNumbers} {
				hash, err := SpecHashFromMap(doc)
				if err != nil {
					t.Fatalf("%s: SpecHashFromMap: %v", label, err)
				}
				if hash != v.SHA256 {
					t.Fatalf("%s: hash = %s, want %s", label, hash, v.SHA256)
				}
			}
		})
	}
}

// The two documents differ only in key order and spacing.
func TestSpecHashVectors_OrderAndSpacingGiveTheSameHash(t *testing.T) {
	a := []byte(`{"steps":[{"id":"a","order":1,"patch":{"spec":{"resources":{"limits":{"cpu":"1","memory":"1Gi"},"requests":{"cpu":"250m"}}}}}],"explanation":"x"}`)
	b := []byte(" {\n \"explanation\" : \"x\", \"steps\" : [ {\"patch\":{\"spec\":{\"resources\":{\"requests\":{\"cpu\":\"250m\"},\"limits\":{\"memory\":\"1Gi\",\"cpu\":\"1\"}}}},\"order\":1,\"id\":\"a\"} ]\n}\n")
	ha, err := SpecHashFromJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := SpecHashFromJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("%s != %s", ha, hb)
	}
}

func TestSpecHashVectors_TheTypedHashEqualsTheHashOfItsOwnJSON(t *testing.T) {
	spec := hashTestSpec()
	typed := mustHash(t, spec)
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	fromRaw, err := SpecHashFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if typed != fromRaw {
		t.Fatalf("typed hash %s != raw hash %s", typed, fromRaw)
	}
}

func TestSpecHashFromJSON_RefusesWhatIsNotAJSONObject(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":          ``,
		"not json":       `{not json`,
		"array":          `[]`,
		"string":         `"x"`,
		"null":           `null`,
		"trailing junk":  `{} {}`,
		"invalid UTF-8":  "{\"explanation\":\"a\xffb\"}",
		"duplicate keys": `{"a":1,"a":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			if h, err := SpecHashFromJSON([]byte(raw)); err == nil {
				t.Fatalf("hashed %q to %s; a spec the API server could not have served must not hash", raw, h)
			}
		})
	}
}
