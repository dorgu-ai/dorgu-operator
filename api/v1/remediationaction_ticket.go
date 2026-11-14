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
	"fmt"
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

	// ReasonTicketIncomplete is the GateRefused reason of a ticket that lacks
	// specHash, approvedBy, issuedAt or expiresAt: a v0 ticket written before
	// ticket v1. It is refused, never honoured.
	ReasonTicketIncomplete = "TicketIncomplete"
)

// CanonicalJSON is the canonical encoding of the spec that SpecHash hashes.
//
// The encoding, which a client in another language can reproduce exactly:
//
//  1. Serialise the spec as JSON with the field names and omitempty rules of this
//     API version (the same JSON the API server stores).
//  2. Parse that JSON into a generic document, keeping numbers as their literal
//     text, so embedded patches (spec.steps[].patch, spec.action.patch) are
//     normalised along with everything else.
//  3. Serialise the document again with object keys sorted by byte order, no
//     whitespace between tokens, no trailing newline, and the characters <, > and
//     & written as themselves (no < escapes). Strings use the standard JSON
//     escapes.
//
// Two documents that differ only in key order or spacing therefore encode alike,
// and any change to a value, a field or a step changes the encoding.
func (s RemediationActionSpec) CanonicalJSON() ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encoding the spec: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc any
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("reading the encoded spec back: %w", err)
	}

	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(doc); err != nil {
		return nil, fmt.Errorf("canonicalising the spec: %w", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// Hash is ApplyTicket.SpecHash for this spec: the SHA-256 of CanonicalJSON, as 64
// lowercase hexadecimal characters with no prefix. It errors when the spec cannot
// be encoded (a patch that is not JSON), which a stored spec never is.
func (s RemediationActionSpec) Hash() (string, error) {
	canonical, err := s.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
