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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gowebpki/jcs"
)

// Ticket v1 (P2-D9). The ticket binds what was cleared to who approved it and for
// how long: ApplyTicket.SpecHash, ApplyTicket.ApprovedBy and ApplyTicket.ExpiresAt.
// The CLI reads these field names, so they are a contract.

const (
	// ReasonTicketExpired is the reason of the Expired condition on a
	// RemediationAction whose ticket ran out before it was applied. The phase is
	// Expired. Nothing was applied, and it is neither a rejection nor a failure.
	ReasonTicketExpired = "TicketExpired"

	// ReasonSpecChanged is the reason of the Applied=False condition on a
	// RemediationAction whose spec no longer hashes to the ticket's specHash. The
	// phase is Failed. Nothing was applied.
	ReasonSpecChanged = "SpecChanged"

	// ReasonTicketUnwritable is the GateRefused reason of an approval whose apply
	// ticket the API server refused to store. The same ticket is refused every
	// time, so the action ends Failed (GateRefusedAtApproval) instead of retrying.
	// Nothing was applied.
	ReasonTicketUnwritable = "TicketUnwritable"

	// ReasonTicketIncomplete is the GateRefused reason of a ticket that lacks
	// specHash, approvedBy, issuedAt or expiresAt: a v0 ticket written before
	// ticket v1. It is refused, never honoured.
	ReasonTicketIncomplete = "TicketIncomplete"
)

// SpecHashAlgorithm is the one-line statement of how ApplyTicket.SpecHash is
// computed. A client in any language can reproduce the hash from this alone:
//
//  1. Take the RemediationAction's .spec object exactly as the API server serves
//     it (kubectl get -o json, or an unstructured client). Never re-encode a typed
//     struct: a client built against another version of this API, or one that does
//     not know a field, would then compute another hash. Provenance
//     (spec.provenance), when present, is part of .spec and so is hashed.
//  2. Canonicalise it as RFC 8785 (JSON Canonicalization Scheme, JCS):
//     - object members are sorted by the UTF-16 code units of their names (not by
//     UTF-8 bytes or code points: U+1F600 sorts before U+E000);
//     - no whitespace between tokens, no trailing newline;
//     - strings are UTF-8 and escape only the quote (\"), the backslash (\\) and
//     the control characters below U+0020, as \b \t \n \f \r or lowercase \u00xx
//     (so U+0000 is \u0000). Every other character is written as itself,
//     including DEL, U+0085, U+2028, U+2029, "<", ">" and "&", and "/" is not
//     escaped;
//     - numbers are the ECMAScript (ES6) shortest round-trip form of the IEEE 754
//     double: 1.0 is 1, 1e3 is 1000, -0 is 0, 1e21 is 1e+21, 1e-7 is 1e-7,
//     12345678901234567890 is 12345678901234567000;
//     - arrays keep their order; true, false and null are written as such.
//  3. The hash is the SHA-256 of those UTF-8 bytes as 64 lowercase hexadecimal
//     characters, with no prefix.
//
// The operator hashes the spec it reads through an unstructured, uncached read of
// the RemediationAction, at ticket issue and again at apply, so a client that
// follows these steps on what it reads computes the operator's hash for any way the
// object was written (kubectl create, server-side apply, the operator's own typed
// write). Golden vectors for every rule above are in
// testdata/spechash_vectors.json, and every implementation should run them.
const SpecHashAlgorithm = "lowercase hex sha256 of the RFC 8785 (JCS) canonical JSON of the RemediationAction .spec as served by the API server"

// errNotAnObject is returned for a document that is not one JSON object.
var errNotAnObject = errors.New("a spec is one JSON object")

// CanonicalSpecJSON returns the RFC 8785 canonical form of a spec given as JSON
// text (the .spec of a RemediationAction as the API server serves it). The input
// must be one JSON object in valid UTF-8 without duplicate member names.
//
// See SpecHashAlgorithm for the exact encoding.
func CanonicalSpecJSON(raw []byte) ([]byte, error) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errNotAnObject
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalising the spec: %w", err)
	}
	return canonical, nil
}

// SpecHashFromJSON is the specHash of a spec given as JSON text: the SHA-256 of
// CanonicalSpecJSON as 64 lowercase hexadecimal characters. It is the function a
// CLI, a dashboard or any other client uses on the spec it read from the API
// server. See SpecHashAlgorithm.
func SpecHashFromJSON(raw []byte) (string, error) {
	canonical, err := CanonicalSpecJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// SpecHashFromMap is SpecHashFromJSON for a spec already decoded into a generic
// document (an unstructured object's "spec"). Numbers may be float64, int64 or
// json.Number.
func SpecHashFromMap(spec map[string]any) (string, error) {
	if spec == nil {
		return "", errNotAnObject
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encoding the spec: %w", err)
	}
	return SpecHashFromJSON(raw)
}
