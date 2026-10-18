package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func Test_runRoute_cachedRoute(t *testing.T) {
	for _, tc := range []struct {
		name            string
		emptyResponse   bool
		noEndpoints     bool
		cacheChange     string
		serverAvailable bool
		wantStatus      int64
		wantQueries     int64
		wantErr         string
	}{
		{name: "fully cached offline"},
		{name: "fully cached without endpoints", noEndpoints: true},
		{name: "empty cached responses offline", emptyResponse: true},
		{name: "missing cache offline", cacheChange: "missing", wantStatus: 1, wantErr: "no overpass servers available"},
		{name: "expired cache offline", cacheChange: "expired", wantStatus: 1, wantErr: "no overpass servers available"},
		{name: "corrupt cache offline", cacheChange: "corrupt", wantErr: "decoding response body"},
		{name: "partially cached online", cacheChange: "missing", serverAvailable: true, wantStatus: 1, wantQueries: 1},
		{name: "expired cache online", cacheChange: "expired", serverAvailable: true, wantStatus: 1, wantQueries: 1},
	} {
		for _, failFast := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/failFast=%t", tc.name, failFast), func(t *testing.T) {
				dir := t.TempDir()
				route := filepath.Join(dir, "route.gpx")
				if err := os.WriteFile(route, []byte(`<gpx version="1.1" creator="test"><trk><trkseg>
<trkpt lat="40.0" lon="-105.0"/>
<trkpt lat="40.1" lon="-105.1"/>
<trkpt lat="40.2" lon="-105.2"/>
</trkseg></trk></gpx>`), 0600); err != nil {
					t.Fatal(err)
				}
				var available atomic.Bool
				available.Store(true)
				var statusRequests, queryRequests atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/status":
						statusRequests.Add(1)
					case "/interpreter":
						queryRequests.Add(1)
					default:
						t.Errorf("unexpected request: %s", r.URL.Path)
						http.NotFound(w, r)
						return
					}
					if !available.Load() {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					content := `{"elements":[]}`
					if r.URL.Path == "/status" {
						content = "Rate limit: 0\n"
					} else if !tc.emptyResponse {
						// Each split returns a distinct POI, so mixed cache/API
						// output must retain both sources to match the warm run.
						id := 1
						if strings.Contains(r.FormValue("data"), "40.000000,-105.000000") {
							id = 2
						}
						content = fmt.Sprintf(`{"elements":[{"type":"node","id":%d,"lat":40.1,"lon":-105.1,"tags":{"name":"Water","amenity":"drinking_water"}}]}`, id)
					}
					if _, err := fmt.Fprint(w, content); err != nil {
						t.Errorf("writing test response: %v", err)
					}
				}))
				defer server.Close()
				endpoints := []endpointSpec{{Name: "test", Interpreter: server.URL + "/interpreter", Status: server.URL + "/status"}}
				cacheDir := filepath.Join(dir, "cache")
				warmOutput := filepath.Join(dir, "warm.geojson")
				if err := runRoute(context.Background(), io.Discard, route, "", 2, 1, 0, failFast, cacheDir, time.Hour, warmOutput, endpoints); err != nil {
					t.Fatalf("populating cache: %v", err)
				}
				if got := queryRequests.Load(); got != 2 {
					t.Fatalf("cache population made %d queries, want 2", got)
				}
				warmData, err := os.ReadFile(warmOutput)
				if err != nil {
					t.Fatal(err)
				}
				var fc featureCollection
				if err := json.Unmarshal(warmData, &fc); err != nil {
					t.Fatal(err)
				}
				wantFeatures := 2
				if tc.emptyResponse {
					wantFeatures = 0
				}
				if fc.Type != "FeatureCollection" || len(fc.Features) != wantFeatures {
					t.Fatalf("unexpected initial output: %s", warmData)
				}

				entries, err := os.ReadDir(cacheDir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 2 {
					t.Fatalf("got %d cache entries, want 2", len(entries))
				}
				cachePath := filepath.Join(cacheDir, entries[0].Name())
				switch tc.cacheChange {
				case "missing":
					err = os.Remove(cachePath)
				case "expired":
					old := time.Now().Add(-2 * time.Hour)
					err = os.Chtimes(cachePath, old, old)
				case "corrupt":
					err = os.WriteFile(cachePath, []byte("{"), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}

				available.Store(tc.serverAvailable)
				statusRequests.Store(0)
				queryRequests.Store(0)
				output := filepath.Join(dir, "result.geojson")
				if tc.noEndpoints {
					endpoints = nil
				}
				err = runRoute(context.Background(), io.Discard, route, "", 2, 1, 0, failFast, cacheDir, time.Hour, output, endpoints)
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("got error %v, want %q", err, tc.wantErr)
					}
					if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("failed run should not create output; stat error: %v", err)
					}
				} else {
					if err != nil {
						t.Fatalf("processing cached route: %v", err)
					}
					data, err := os.ReadFile(output)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(data, warmData) {
						t.Fatalf("output changed after caching:\ngot: %s\nwant: %s", data, warmData)
					}
				}
				if got := statusRequests.Load(); got != tc.wantStatus {
					t.Errorf("got %d status requests, want %d", got, tc.wantStatus)
				}
				if got := queryRequests.Load(); got != tc.wantQueries {
					t.Errorf("got %d interpreter requests, want %d", got, tc.wantQueries)
				}
			})
		}
	}
}

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
