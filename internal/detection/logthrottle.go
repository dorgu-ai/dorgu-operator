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

package detection

import (
	"sync"
	"time"
)

// logRepeatInterval is how often one key may log again while its condition holds.
// A collector that stays broken (a metrics API that is not installed, a Prometheus
// that is down) is checked every cycle, and a line per cycle is a thousand a day
// that bury everything else. What is rate-limited is only the log line: the error
// still reaches every caller of CollectAll, every cycle.
const logRepeatInterval = 5 * time.Minute

// logThrottle lets the first occurrence of a key through, then at most one more per
// interval, and says how many it held back, so nothing is silently dropped.
type logThrottle struct {
	mu         sync.Mutex
	interval   time.Duration
	last       map[string]time.Time
	suppressed map[string]int
}

func newLogThrottle(interval time.Duration) *logThrottle {
	return &logThrottle{
		interval:   interval,
		last:       map[string]time.Time{},
		suppressed: map[string]int{},
	}
}

// allow reports whether key may log at now, and how many occurrences were held back
// since it last did.
func (l *logThrottle) allow(key string, now time.Time) (bool, int) {
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
