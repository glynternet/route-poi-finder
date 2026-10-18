package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	gpxgo "github.com/tkrajina/gpxgo/gpx"
)

type lookupRecordingCache struct {
	queryCache
	lookups []string
}

func (c *lookupRecordingCache) Lookup(query string) ([]element, bool, error) {
	c.lookups = append(c.lookups, query)
	return c.queryCache.Lookup(query)
}

func Test_queryOrchestrator_clientsRegisterAsReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstQueryStarted := make(chan struct{})
	secondQueryStarted := make(chan struct{})
	slowStatusStarted := make(chan struct{})
	slowStatusCancelled := make(chan struct{})
	var firstQueries, secondQueries atomic.Int64

	// The first server starts work while the second is still provisioning.
	// It then waits for the second server to join and take the remaining job.
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := "Rate limit: 0\n"
		if r.URL.Path == "/interpreter" {
			firstQueries.Add(1)
			close(firstQueryStarted)
			select {
			case <-secondQueryStarted:
			case <-r.Context().Done():
				return
			}
			content = `{"elements":[{"type":"node","id":1}]}`
		}
		if _, err := fmt.Fprint(w, content); err != nil {
			t.Errorf("writing first response: %v", err)
		}
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := "Rate limit: 0\n"
		if r.URL.Path == "/status" {
			select {
			case <-firstQueryStarted:
			case <-r.Context().Done():
				return
			}
		} else {
			secondQueries.Add(1)
			// Ensure the third server has begun provisioning before work
			// finishes, so its cancellation is exercised deterministically.
			select {
			case <-slowStatusStarted:
			case <-r.Context().Done():
				return
			}
			close(secondQueryStarted)
			content = `{"elements":[{"type":"node","id":2}]}`
		}
		if _, err := fmt.Fprint(w, content); err != nil {
			t.Errorf("writing second response: %v", err)
		}
	}))
	defer second.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(slowStatusStarted)
		<-r.Context().Done()
		close(slowStatusCancelled)
	}))
	defer slow.Close()
	endpoints := []endpointSpec{
		{Name: "first", Interpreter: first.URL + "/interpreter", Status: first.URL + "/status", Concurrency: 1},
		{Name: "second", Interpreter: second.URL + "/interpreter", Status: second.URL + "/status", Concurrency: 1},
		{Name: "slow", Interpreter: slow.URL + "/interpreter", Status: slow.URL + "/status", Concurrency: 1},
	}
	cache := &lookupRecordingCache{queryCache: fileQueryCache{dir: t.TempDir(), ttl: time.Hour}}
	o := queryOrchestrator{
		cache:        cache,
		queryTimeout: time.Second,
		failFast:     true,
		startClients: func(ctx context.Context) (<-chan clientWorkers, func()) {
			return provisionClients(ctx, endpoints, time.Second)
		},
	}
	units := []workUnit{
		{splitIndex: 0, queries: queries, routePoints: []gpxgo.GPXPoint{{Point: gpxgo.Point{Latitude: 40, Longitude: -105}}}},
		{splitIndex: 1, queries: queries, routePoints: []gpxgo.GPXPoint{{Point: gpxgo.Point{Latitude: 41, Longitude: -106}}}},
	}
	results, err := o.Run(ctx, units)
	if err != nil {
		t.Fatalf("processing route: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("processing waited for the parent deadline")
	}
	if len(results) != 2 || firstQueries.Load() != 1 || secondQueries.Load() != 1 {
		t.Fatalf("got %d results, %d first queries, %d second queries; want 2, 1, 1", len(results), firstQueries.Load(), secondQueries.Load())
	}
	var ids []int64
	for _, result := range results {
		for _, node := range result.nodes {
			ids = append(ids, node.ID)
		}
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{1, 2}) {
		t.Fatalf("got node IDs %v, want [1 2]", ids)
	}
	if len(cache.lookups) != 2 || cache.lookups[0] == cache.lookups[1] {
		t.Fatalf("want one cache lookup per distinct prepared query, got %d lookups", len(cache.lookups))
	}
	select {
	case <-slowStatusCancelled:
	case <-ctx.Done():
		t.Fatal("unfinished provisioning was not cancelled")
	}

	// Every response was written back to the cache. Replaying the same route
	// must neither provision clients nor perform extra cache lookups in workers.
	cache.lookups = nil
	o.startClients = func(context.Context) (<-chan clientWorkers, func()) {
		t.Error("fully cached route started clients")
		return readyClients(), func() {}
	}
	results, err = o.Run(ctx, units)
	if err != nil || len(results) != 2 || len(cache.lookups) != 2 {
		t.Fatalf("cached replay: %d results, %d lookups, error %v", len(results), len(cache.lookups), err)
	}
}

func Test_concurrentUnitsWorker_noClients(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	process := concurrentUnitsWorker(ctx, cancel, readyClients(), func(namedClient, int) (int, error) {
		t.Error("work executed without a client")
		return 0, nil
	}, true, 0)
	results, err := process(1)
	if err == nil || len(results) != 0 {
		t.Fatalf("got %v results and error %v, want unavailable server error", results, err)
	}
}
