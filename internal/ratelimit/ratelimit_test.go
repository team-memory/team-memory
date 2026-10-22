package ratelimit

import (
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(2, 3).WithClock(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d within burst refused", i)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("over burst: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("keys must not share buckets")
	}
	now = now.Add(500 * time.Millisecond)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("refill at 2/s should allow one request after 500ms")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("only one token should have refilled")
	}
}

func TestDisabled(t *testing.T) {
	l := New(0, 1)
	for i := 0; i < 100; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatal("rate 0 disables limiting")
		}
	}
}
