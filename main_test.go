package main

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func Test_orderRetainingUniqCompact(t *testing.T) {
	actual := slices.CompactFunc([]string{"a", "a", "b", "c", "a", "b", "b"}, orderRetainingUniqCompact[string]())
	expected := []string{"a", "b", "c"}
	if !slices.Equal(expected, actual) {
		t.Fatalf("Expected %v to be %v", expected, actual)
	}
}

// With no units, the worker must return immediately without processing anything
// and without blocking on clientsReady (which here is never fed or closed), and
// it must cancel the shared context so in-flight provisioning can unwind.
func Test_concurrentUnitsWorker_noUnits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientsReady := make(chan clientWorkers) // deliberately never sent to or closed

	process := concurrentUnitsWorker(
		ctx, cancel, clientsReady,
		func(namedClient, int) (int, error) {
			t.Error("processUnit must not be called when there are no units")
			return 0, nil
		},
		true,
		0,
	)

	done := make(chan struct{})
	var results []int
	var err error
	go func() {
		results, err = process()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process() blocked with no units")
	}

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %v", results)
	}
	if ctx.Err() == nil {
		t.Fatal("expected the shared context to be cancelled")
	}
}

// concurrencyTracker records the peak number of processUnit calls running at
// once, so a test can assert the global --workers cap is respected.
type concurrencyTracker struct {
	inFlight int64
	peak     int64
}

func (c *concurrencyTracker) enter() {
	n := atomic.AddInt64(&c.inFlight, 1)
	for {
		peak := atomic.LoadInt64(&c.peak)
		if n <= peak || atomic.CompareAndSwapInt64(&c.peak, peak, n) {
			break
		}
	}
}

func (c *concurrencyTracker) leave() { atomic.AddInt64(&c.inFlight, -1) }

func readyClients(specs ...clientWorkers) <-chan clientWorkers {
	ch := make(chan clientWorkers, len(specs))
	for _, s := range specs {
		ch <- s
	}
	close(ch)
	return ch
}

// With --workers set, no more than that many units may run concurrently across
// all clients, even when the clients' combined capacity is higher.
func Test_concurrentUnitsWorker_globalCapRespected(t *testing.T) {
	const workers = 3
	const nUnits = 60

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clients := readyClients(
		clientWorkers{client: namedClient{name: "a"}, capacity: 4},
		clientWorkers{client: namedClient{name: "b"}, capacity: 4}, // combined 8 > workers
	)

	var track concurrencyTracker
	process := concurrentUnitsWorker(
		ctx, cancel, clients,
		func(namedClient, int) (int, error) {
			track.enter()
			time.Sleep(2 * time.Millisecond)
			track.leave()
			return 0, nil
		},
		false, workers,
	)

	results, err := process(make([]int, nUnits)...)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != nUnits {
		t.Fatalf("processed %d units, want %d", len(results), nUnits)
	}
	if peak := atomic.LoadInt64(&track.peak); peak > workers {
		t.Fatalf("peak concurrency %d exceeded --workers cap %d", peak, workers)
	}
	if peak := atomic.LoadInt64(&track.peak); peak < 2 {
		t.Fatalf("peak concurrency %d shows work was serialised, not parallel", peak)
	}
}

// With --workers 0 (uncapped), concurrency should be able to exceed any single
// client's capacity and reach the sum of the clients' capacities.
func Test_concurrentUnitsWorker_uncappedUsesFullCapacity(t *testing.T) {
	const nUnits = 60

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clients := readyClients(
		clientWorkers{client: namedClient{name: "a"}, capacity: 4},
		clientWorkers{client: namedClient{name: "b"}, capacity: 4}, // combined 8
	)

	var track concurrencyTracker
	process := concurrentUnitsWorker(
		ctx, cancel, clients,
		func(namedClient, int) (int, error) {
			track.enter()
			time.Sleep(2 * time.Millisecond)
			track.leave()
			return 0, nil
		},
		false, 0, // uncapped
	)

	results, err := process(make([]int, nUnits)...)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != nUnits {
		t.Fatalf("processed %d units, want %d", len(results), nUnits)
	}
	// A single client caps at 4; exceeding that proves work spread across clients
	// with no artificial global cap.
	if peak := atomic.LoadInt64(&track.peak); peak <= 4 {
		t.Fatalf("peak concurrency %d did not exceed a single client's capacity", peak)
	}
}

// failFast must cancel the shared context on the first error, which is what
// aborts in-flight queries/retries/slot-waits in the real pipeline.
func Test_concurrentUnitsWorker_failFastCancelsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clients := readyClients(clientWorkers{client: namedClient{name: "a"}, capacity: 2})

	wantErr := errors.New("boom")
	process := concurrentUnitsWorker(
		ctx, cancel, clients,
		func(namedClient, int) (int, error) { return 0, wantErr },
		true, 0,
	)

	_, err := process(make([]int, 20)...)
	if err == nil {
		t.Fatal("expected an error from failFast run")
	}
	if ctx.Err() == nil {
		t.Fatal("failFast did not cancel the shared context")
	}
}
