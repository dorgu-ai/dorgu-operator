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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

// incidentWriteBackoff paces retries of an IncidentMemory write.
//
// It is slower and longer than retry.DefaultRetry (5 steps over roughly 50ms)
// because the race it covers is a cached read that has not yet caught up with a
// write that already succeeded. Informer caches settle in tens to hundreds of
// milliseconds, not microseconds, so a backoff tuned for optimistic-concurrency
// conflicts gives up long before the object shows up.
var incidentWriteBackoff = wait.Backoff{
	Steps:    6,
	Duration: 50 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
}

// retriableIncidentWriteError reports whether an error is one of the two races
// an incident write has to survive.
//
// Conflict is the familiar one: another controller wrote the object between our
// read and our write. NotFound is the one CF4-2 missed, and it is the one that
// actually cost the clean-room run five of its first six diagnoses (F-05).
// Reads go through the manager's cache, so the Get straight after a successful
// Create can miss an object the API server already holds. The retry that was
// supposed to protect the write only covered Conflict, so a cache that was a
// few milliseconds behind discarded a diagnosis the user had already paid for.
func retriableIncidentWriteError(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsNotFound(err)
}

// retryIncidentWrite runs write until it succeeds or stops failing for a
// reason worth retrying. The attempt number is passed through so a caller that
// already holds a fresh object can skip the re-fetch on the first try and
// re-fetch only once something has actually gone wrong.
func retryIncidentWrite(write func(attempt int) error) error {
	attempt := 0
	return retry.OnError(incidentWriteBackoff, retriableIncidentWriteError, func() error {
		err := write(attempt)
		attempt++
		return err
	})
}
