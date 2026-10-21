package jobs

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	base, max := 5*time.Second, 10*time.Minute
	for attempt, want := range map[int]time.Duration{
		0: 5 * time.Second, 1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second,
		7: 320 * time.Second, 8: 10 * time.Minute, 200: 10 * time.Minute,
	} {
		if got := Backoff(attempt, base, max); got != want {
			t.Errorf("attempt %d: got %s, want %s", attempt, got, want)
		}
	}
	w := NewWorker(nil, "embed", nil, Config{BackoffBase: base, BackoffMax: max})
	for range 100 {
		if d := w.backoff(3); d < 16*time.Second || d > 24*time.Second {
			t.Fatalf("jitter outside +-20%%: %s", d)
		}
	}
}
