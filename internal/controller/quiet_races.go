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
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dorgu-ai/dorgu-operator/internal/k8srace"
)

// raceRequeueDelay is how soon an object is looked at again after an optimistic
// lock conflict. The next attempt re-reads the object, so it only has to be long
// enough for the competing write to land in the cache.
const raceRequeueDelay = time.Second

func isBenignConflict(err error) bool { return k8srace.IsConflict(err) }
func isBenignGone(err error) bool     { return k8srace.IsGone(err) }

// logFailure logs err at ERROR unless it is a benign race, which is logged at
// V(1). Real failures stay loud; the races a busy cluster produces every minute
// do not trip an alert keyed on ERROR lines.
func logFailure(log logr.Logger, err error, msg string, keysAndValues ...any) {
	if k8srace.IsRace(err) {
		log.V(1).Info(msg+" (benign race, will retry)",
			append([]any{"reason", err.Error()}, keysAndValues...)...)
		return
	}
	log.Error(err, msg, keysAndValues...)
}

// quietRaces turns the outcome of a Reconcile into what controller-runtime
// should see. A returned error is logged at ERROR and counted in
// controller_runtime_reconcile_errors_total, so an expected race must not be
// returned as one: a conflict requeues shortly, and an object that is gone has
// nothing left to reconcile. Anything else is returned untouched.
func quietRaces(log logr.Logger, res ctrl.Result, err error) (ctrl.Result, error) {
	switch {
	case err == nil:
		return res, nil
	case isBenignConflict(err):
		log.V(1).Info("conflict writing a status or label, requeueing", "reason", err.Error())
		return ctrl.Result{RequeueAfter: raceRequeueDelay}, nil
	case isBenignGone(err):
		log.V(1).Info("object went away mid-reconcile, nothing to do", "reason", err.Error())
		return ctrl.Result{}, nil
	default:
		return res, err
	}
}
