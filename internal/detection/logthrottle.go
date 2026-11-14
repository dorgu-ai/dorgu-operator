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

// LogRepeatInterval is how often one key may log again while its condition holds.
// A collector that stays broken (a metrics API that is not installed, a Prometheus
// that is down) is checked every cycle, and a line per cycle is a thousand a day
// that bury everything else. What is rate-limited is only the log line: the error
// still reaches every caller of CollectAll, every cycle.
const LogRepeatInterval = 5 * time.Minute

// maxThrottleKeys bounds the memory of a LogThrottle whose keys carry error text.
const maxThrottleKeys = 256

// LogThrottle lets the first occurrence of a key through, then at most one more per
// interval, and says how many it held back, so nothing is silently dropped. The zero
// value is ready to use and throttles at LogRepeatInterval.
type LogThrottle struct {
	mu         sync.Mutex
	interval   time.Duration
	last       map[string]time.Time
	suppressed map[string]int
}

// NewLogThrottle builds a LogThrottle that lets a key through once per interval.
func NewLogThrottle(interval time.Duration) *LogThrottle {
	return &LogThrottle{
		interval:   interval,
		last:       map[string]time.Time{},
		suppressed: map[string]int{},
	}
}

// Allow reports whether key may log at now, and how many occurrences were held back
// since it last did.
func (l *LogThrottle) Allow(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last, l.suppressed = map[string]time.Time{}, map[string]int{}
	}
	if l.interval <= 0 {
		l.interval = LogRepeatInterval
	}
	if last, seen := l.last[key]; seen && now.Sub(last) < l.interval {
		l.suppressed[key]++
		return false, 0
	}
	if len(l.last) >= maxThrottleKeys {
		// Keys can embed error text, which is unbounded. Entries older than the
		// interval would be let through anyway, so dropping them loses nothing.
		for k, at := range l.last {
			if now.Sub(at) >= l.interval {
				delete(l.last, k)
				delete(l.suppressed, k)
			}
		}
	}
	held := l.suppressed[key]
	l.last[key] = now
	l.suppressed[key] = 0
	return true, held
}
