package main

import (
	"sync/atomic"
	"testing"
	"time"
)

func awaitCollectionSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestScheduledCollectionRunsImmediatelyAndUsesExistingCadence(t *testing.T) {
	ticks := make(chan time.Time)
	started := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runScheduledCollection(ticks, func() { started <- struct{}{} })
	}()
	t.Cleanup(func() { close(ticks); awaitCollectionSignal(t, done, "scheduler shutdown") })

	awaitCollectionSignal(t, started, "immediate initial collection")
	select {
	case <-started:
		t.Fatal("collection ran again without a scheduled tick")
	default:
	}
	// A tick is the next scheduled start, not a new delay measured from the
	// previous callback's completion. No real five-minute sleep is required.
	for i := 0; i < 3; i++ {
		select {
		case ticks <- time.Unix(int64(i*300), 0):
		case <-time.After(time.Second):
			t.Fatal("scheduler did not wait on the existing tick source")
		}
		awaitCollectionSignal(t, started, "scheduled collection")
	}
}

func TestScheduledCollectionSerializesOverrunsAndCoalescesMissedTicks(t *testing.T) {
	// A one-slot source models time.Ticker: ticks that cannot be delivered are
	// dropped, so a slow collection can retain at most one catch-up tick.
	ticks := make(chan time.Time, 1)
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	done := make(chan struct{})
	var active atomic.Int32
	var overlapped atomic.Bool
	go func() {
		defer close(done)
		runScheduledCollection(ticks, func() {
			if active.Add(1) != 1 {
				overlapped.Store(true)
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
		})
	}()
	t.Cleanup(func() {
		close(release)
		close(ticks)
		awaitCollectionSignal(t, done, "overrun scheduler shutdown")
	})

	awaitCollectionSignal(t, started, "initial blocking collection")
	for i := 0; i < 20; i++ {
		select {
		case ticks <- time.Unix(int64(i*300), 0):
		default:
		}
	}
	select {
	case <-started:
		t.Fatal("scheduled tick started a concurrent collection")
	default:
	}
	release <- struct{}{}
	awaitCollectionSignal(t, started, "single retained catch-up collection")
	release <- struct{}{}
	// After the retained tick finishes, deliver a new tick. An unbuffered
	// callback barrier and the empty tick channel ensure no missed-tick queue
	// can drive additional collections before this new scheduled start.
	if got := len(ticks); got != 0 {
		t.Fatalf("missed ticks accumulated: %d", got)
	}
	select {
	case <-started:
		t.Fatal("more than one catch-up collection was retained")
	default:
	}
	ticks <- time.Unix(21*300, 0)
	awaitCollectionSignal(t, started, "next regular collection")
	if overlapped.Load() {
		t.Fatal("collection callbacks overlapped")
	}
}
