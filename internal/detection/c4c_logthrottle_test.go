package detection

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// C4c I-3: a throttle keyed by incident must not keep the keys of incidents that are gone
// until it happens to reach its size cap.

func TestLogThrottle_RetainDropsTheKeysNotKept(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	l := NewLogThrottle(time.Hour)
	for _, key := range []string{"a", "b", "c"} {
		ok, _ := l.Allow(key, now)
		assert.True(t, ok)
	}
	assert.Equal(t, 3, l.Len())

	l.Retain(func(key string) bool { return key == "b" })
	assert.Equal(t, 1, l.Len())

	ok, _ := l.Allow("b", now.Add(time.Minute))
	assert.False(t, ok, "a kept key is still throttled")
	ok, held := l.Allow("a", now.Add(time.Minute))
	assert.True(t, ok, "a dropped key starts over: the next occurrence logs")
	assert.Zero(t, held)
}

func TestLogThrottle_RetainOnTheZeroValueAndOnNil(t *testing.T) {
	var l LogThrottle
	assert.Zero(t, l.Len())
	assert.NotPanics(t, func() { l.Retain(func(string) bool { return false }) })
	assert.NotPanics(t, func() { l.Retain(nil) }, "nil keeps nothing back, and does not crash")
}

func TestLogThrottle_RetainForgetsTheHeldBackCountToo(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	l := NewLogThrottle(time.Hour)
	l.Allow("a", now)
	l.Allow("a", now.Add(time.Second)) // held back
	l.Retain(func(string) bool { return false })
	ok, held := l.Allow("a", now.Add(2*time.Second))
	assert.True(t, ok)
	assert.Zero(t, held, "the count belonged to the incident that is gone")
}
