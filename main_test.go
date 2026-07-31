package main

import (
	"context"
	"slices"
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
