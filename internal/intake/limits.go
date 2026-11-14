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

package intake

import (
	"context"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
	"github.com/dorgu-ai/dorgu-operator/internal/safetext"
)

// maxUsernameLength is the CRD's bound on proposer.username, in characters.
const maxUsernameLength = 253

// cleanUsername is a proposer's claimed username as the operator records and
// prints it: stripped of control and invisible characters and cut to the CRD's
// bound. It is also what proposals are compared by, so characters nobody can see
// do not make one proposer into two.
func cleanUsername(username string) string {
	cleaned := safetext.Clean(username, false)
	if utf8.RuneCountInString(cleaned) <= maxUsernameLength {
		return cleaned
	}
	return string([]rune(cleaned)[:maxUsernameLength])
}

// listProposals reads every proposal in the cluster (from the cache, see Config.Lister). The rate limit counts across
// namespaces, because one proposer is one identity wherever it writes.
//
// The List is read-only (UnsafeDisableDeepCopy): the cache hands out its own objects
// instead of a copy of every proposal in the cluster for each one decided. Nothing
// here or in earlierDuplicate and countRecentBefore writes to an item.
func (p *Processor) listProposals(ctx context.Context) ([]v1alpha1.RemediationProposal, error) {
	var list v1alpha1.RemediationProposalList
	if err := p.lister.List(ctx, &list, client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("listing RemediationProposals: %w", err)
	}
	return list.Items, nil
}

// same reports whether two proposals are the one object.
func same(a, b *v1alpha1.RemediationProposal) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name
}

// before orders proposals by arrival: creation time, then name. It is a total
// order, so every reconcile of a proposal sees the same set ahead of it and the
// answer does not depend on when it runs.
func before(a, b *v1alpha1.RemediationProposal) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// earlierDuplicate returns the name of the earliest proposal in the same namespace
// with the same proposer and idempotency key that arrived before prop, or "".
//
// Only a proposal that holds the key counts: one that is Received (undecided) or
// Converted. A Rejected proposal released it, so the retry that an idempotency key
// exists to make safe (after a rate limit, a missing incident or a gate refusal the
// proposer has since fixed) is decided on its merits instead of refused as its own
// duplicate until the Rejected proposal is pruned.
func earlierDuplicate(prop *v1alpha1.RemediationProposal, all []v1alpha1.RemediationProposal) string {
	var dups []*v1alpha1.RemediationProposal
	for i := range all {
		other := &all[i]
		if same(other, prop) || other.Namespace != prop.Namespace ||
			cleanUsername(other.Spec.Proposer.Username) != cleanUsername(prop.Spec.Proposer.Username) ||
			other.Spec.IdempotencyKey != prop.Spec.IdempotencyKey ||
			other.Status.Phase == v1alpha1.PhaseRejected {
			continue
		}
		if before(other, prop) {
			dups = append(dups, other)
		}
	}
	if len(dups) == 0 {
		return ""
	}
	sort.Slice(dups, func(i, j int) bool { return before(dups[i], dups[j]) })
	return dups[0].Name
}

// countRecentBefore counts the proposals by prop's proposer that arrived before it
// and no more than window earlier: how many it had already had received.
func countRecentBefore(prop *v1alpha1.RemediationProposal, all []v1alpha1.RemediationProposal, window time.Duration) int {
	cutoff := prop.CreationTimestamp.Add(-window)
	n := 0
	for i := range all {
		other := &all[i]
		if same(other, prop) || cleanUsername(other.Spec.Proposer.Username) != cleanUsername(prop.Spec.Proposer.Username) {
			continue
		}
		if before(other, prop) && other.CreationTimestamp.After(cutoff) {
			n++
		}
	}
	return n
}
