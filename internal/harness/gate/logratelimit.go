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

package gate

import (
	"sync"
	"time"
)

// unreadableLogInterval is how often one unreadable-ClusterPersona condition may
// log at ERROR. While the ClusterPersona cannot be read (lost RBAC, missing CRD)
// every proposal and every model call asks again, once per incident per cycle, so
// an unthrottled log buries everything else. The refusal itself is not throttled:
// only the log line is.
const unreadableLogInterval = 5 * time.Minute

// logLimiter lets the first occurrence of each key through, then at most one more
// per interval, and says how many it held back so nothing is silently dropped.
type logLimiter struct {
	mu         sync.Mutex
	interval   time.Duration
	last       map[string]time.Time
	suppressed map[string]int
}

func newLogLimiter(interval time.Duration) *logLimiter {
	return &logLimiter{
		interval:   interval,
		last:       map[string]time.Time{},
		suppressed: map[string]int{},
	}
}

// allow reports whether the key may log now, and how many occurrences were held
// back since it last did.
func (l *logLimiter) allow(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if last, seen := l.last[key]; seen && now.Sub(last) < l.interval {
		l.suppressed[key]++
		return false, 0
	}
	held := l.suppressed[key]
	l.last[key] = now
	l.suppressed[key] = 0
	return true, held
}

// logUnreadable logs err at ERROR for a ClusterPersona read failure, rate-limited
// per site. The first failure logs at once; repeats within the interval are
// counted and reported on the next line that gets through.
func (g *Gate) logUnreadable(site string, err error, msg string, kv ...any) {
	ok, held := g.logLimit.allow(site, g.now())
	if !ok {
		return
	}
	if held > 0 {
		kv = append(kv, "suppressedSinceLastLog", held)
	}
	g.logger.Error(err, msg, kv...)
}
