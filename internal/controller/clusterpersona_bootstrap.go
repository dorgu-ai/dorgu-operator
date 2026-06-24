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

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	bootstrapPersonaName = "dorgu-cluster"
	annotationClusterUID = "dorgu.io/cluster-uid"
	annotationBootstrap  = "dorgu.io/bootstrap"

	// defaultEnsureInterval is the period of the periodic ensure loop when none is
	// configured. It bounds the worst-case latency for the persona to appear if the
	// initial attempt (or a later one) failed transiently.
	defaultEnsureInterval = 2 * time.Minute
	// minEnsureInterval clamps the configured interval so the ensure loop can never
	// hot-loop against the API server.
	minEnsureInterval = 30 * time.Second
)

// bootstrapBackoff bounds the retry budget for a single ensure attempt. With these
// parameters the total wall-clock spent retrying a transient error is roughly
// 1+2+4+8+16 ≈ 31s (capped per-step at 30s), after which we give up for this cycle
// and let the periodic ensure loop try again next interval.
var bootstrapBackoff = wait.Backoff{
	Duration: 1 * time.Second,
	Factor:   2.0,
	Jitter:   0.1,
	Steps:    5,
	Cap:      30 * time.Second,
}

// ClusterPersonaBootstrap is a manager.Runnable that auto-creates a default
// ClusterPersona ("dorgu-cluster") when none exists.
//
// It is layered for reliability (defense-in-depth):
//   - the initial run (fast path) ensures the persona promptly once the leader lease
//     is acquired and the cache is synced;
//   - a periodic ensure loop re-checks presence every EnsureInterval so the persona
//     converges even if the fast path missed (transient API error, lease flap);
//   - each attempt retries transient List/Create errors under a bounded backoff
//     instead of swallowing them, so a single startup blip no longer leaves the
//     persona permanently absent.
//
// It is idempotent: if a persona already exists (or AlreadyExists is returned on
// create, e.g. from a concurrent leader replica) it returns without error and
// without creating a duplicate. All failures are non-fatal so the operator keeps
// running regardless.
type ClusterPersonaBootstrap struct {
	Client client.Client
	Log    logr.Logger
	// EnsureInterval is how often the periodic ensure loop re-checks persona presence.
	// Zero/unset falls back to defaultEnsureInterval; values below minEnsureInterval
	// are clamped up to minEnsureInterval.
	EnsureInterval time.Duration
}

// NeedLeaderElection keeps the bootstrap leader-gated so multiple replicas don't
// race to create the persona. The periodic ensure (every EnsureInterval) bounds
// the lease-acquisition latency window that previously made the persona "empty
// for the first few minutes" (Increment-0 Finding 4). AlreadyExists on Create is
// treated as success, so even if this ran non-leader it would be safe.
//
// The chart defaults to a single replica today; this choice is already correct for
// HA/multi-replica. If the operator is ever guaranteed single-replica, returning
// false would remove the lease-wait latency entirely — but we default to true and
// make the choice explicit rather than relying on controller-runtime's silent
// "leader-gated unless LeaderElectionRunnable says otherwise" default.
func (b *ClusterPersonaBootstrap) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It is invoked by controller-runtime after the
// manager cache is synced and (because NeedLeaderElection returns true) after the
// leader lease is acquired. It blocks until ctx is cancelled, running the periodic
// ensure loop. Always returns nil — failures are logged, never propagated, so a
// bootstrap problem never crashes the manager.
func (b *ClusterPersonaBootstrap) Start(ctx context.Context) error {
	interval := b.ensureInterval()
	b.Log.Info("ClusterPersona bootstrap running (leader acquired); starting periodic ensure",
		"interval", interval.String())

	// wait.UntilWithContext runs the function immediately, then every interval until
	// ctx is cancelled. The immediate first run is the fast path (~seconds after the
	// lease is won); subsequent ticks are the self-healing safety net. ensure is
	// idempotent and cheap when the persona already exists, so the loop is nearly free.
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := b.ensureWithRetry(ctx); err != nil {
			b.Log.Error(err, "failed to ensure ClusterPersona this cycle; will retry next interval")
		}
	}, interval)

	return nil
}

// ensureInterval resolves the effective periodic interval, applying the default and
// the minimum clamp.
func (b *ClusterPersonaBootstrap) ensureInterval() time.Duration {
	switch {
	case b.EnsureInterval <= 0:
		return defaultEnsureInterval
	case b.EnsureInterval < minEnsureInterval:
		return minEnsureInterval
	default:
		return b.EnsureInterval
	}
}

// ensureWithRetry runs ensure under a bounded exponential backoff. A transient
// List/Create error at startup no longer causes the persona to be permanently
// absent: it is retried within the backoff budget. If every attempt fails, the
// error is returned (and logged by the caller) but the operator keeps running and
// the next periodic tick will try again.
func (b *ClusterPersonaBootstrap) ensureWithRetry(ctx context.Context) error {
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, bootstrapBackoff,
		func(ctx context.Context) (bool, error) {
			if err := b.ensure(ctx); err != nil {
				lastErr = err
				b.Log.V(1).Info("ensure attempt failed, will retry", "error", err.Error())
				return false, nil
			}
			return true, nil
		})
	if err != nil {
		if lastErr != nil {
			return fmt.Errorf("ensuring ClusterPersona after retries: %w", lastErr)
		}
		// ctx cancelled before any attempt completed.
		return fmt.Errorf("ensuring ClusterPersona: %w", err)
	}
	return nil
}

// ensure creates the dorgu-cluster ClusterPersona if none exists. It is idempotent:
// when a persona already exists, or Create returns AlreadyExists (a concurrent
// replica won the race), it returns nil. It returns a non-nil error only on a
// transient failure the caller may retry.
func (b *ClusterPersonaBootstrap) ensure(ctx context.Context) error {
	list := &dorguv1.ClusterPersonaList{}
	if err := b.Client.List(ctx, list); err != nil {
		return fmt.Errorf("listing ClusterPersonas: %w", err)
	}
	if len(list.Items) > 0 {
		b.Log.V(1).Info("ClusterPersona already exists, nothing to do", "name", list.Items[0].Name)
		return nil
	}

	// The kube-system namespace UID is a stable per-cluster identity anchor, stored as
	// an annotation for future multi-cluster use. It is best-effort: if it is
	// unavailable we still create the persona, just without the annotation.
	clusterUID := ""
	ns := &corev1.Namespace{}
	if err := b.Client.Get(ctx, types.NamespacedName{Name: "kube-system"}, ns); err != nil {
		b.Log.Error(err, "failed to get kube-system namespace, creating persona without cluster-uid annotation")
	} else {
		clusterUID = string(ns.UID)
	}

	persona := b.buildPersona(clusterUID)
	if err := b.Client.Create(ctx, persona); err != nil {
		if errors.IsAlreadyExists(err) {
			// A concurrent leader replica created it between our List and Create.
			b.Log.Info("ClusterPersona created by concurrent replica, nothing to do")
			return nil
		}
		return fmt.Errorf("creating ClusterPersona %q: %w", bootstrapPersonaName, err)
	}

	b.Log.Info("Auto-created ClusterPersona",
		"name", bootstrapPersonaName,
		"clusterUID", clusterUID)
	return nil
}

func (b *ClusterPersonaBootstrap) buildPersona(clusterUID string) *dorguv1.ClusterPersona {
	annotations := map[string]string{
		annotationBootstrap: "true",
	}
	if clusterUID != "" {
		annotations[annotationClusterUID] = clusterUID
	}

	trustLevel := int32(2)
	maxRemPerHour := int32(5)

	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:        bootstrapPersonaName,
			Annotations: annotations,
		},
		Spec: dorguv1.ClusterPersonaSpec{
			Name:        bootstrapPersonaName,
			Description: "Auto-created by Dorgu Operator on startup",
			Environment: "development",
			Policies: &dorguv1.ClusterPolicies{
				SelfHealing: &dorguv1.SelfHealingPolicy{
					Enabled:                true,
					Mode:                   "observe",
					TrustLevel:             trustLevel,
					MaxRemediationsPerHour: maxRemPerHour,
				},
			},
		},
	}
}
