package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/glynternet/route-poi-finder/overpass"
)

// namedClient pairs an Overpass client with the human-readable endpoint name
// used for logging which server handled which split.
type namedClient struct {
	name   string
	client *overpass.Client
}

// clientWorkers binds a named client to its capacity: the number of worker
// goroutines (concurrent query slots) it offers, which pull from the shared jobs
// channel and run queries through it. For rate-limited servers this is the rate
// limit; for unlimited servers it is the configured concurrency.
type clientWorkers struct {
	client   namedClient
	capacity int
}

// retryConfig holds retry settings
type retryConfig struct {
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
}

// httpStatusError wraps HTTP status code errors for retry logic
type httpStatusError struct {
	statusCode int
	status     string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("unexpected status code %d (%s)", e.statusCode, e.status)
}

// isRetryableError determines if an error should trigger a retry
func isRetryableError(err error) bool {
	// Network errors are retryable
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// Check for HTTP status code errors
	var httpErr *httpStatusError
	if errors.As(err, &httpErr) {
		return httpErr.statusCode >= 500 || httpErr.statusCode == 429
	}

	return false
}

func retrier[T any](conf retryConfig) func(ctx context.Context, queryFn func() (T, error)) (T, error) {
	return func(ctx context.Context, queryFn func() (T, error)) (T, error) {
		var result T
		var lastErr error

		for attempt := 0; attempt <= conf.maxRetries; attempt++ {
			if attempt > 0 {
				delay := conf.baseDelay * time.Duration(1<<(attempt-1))
				if delay > conf.maxDelay {
					delay = conf.maxDelay
				}

				select {
				case <-ctx.Done():
					return result, ctx.Err()
				case <-time.After(delay):
				}

				log.Printf("Retry attempt %d/%d after error: %v", attempt, conf.maxRetries, lastErr)
			}

			result, lastErr = queryFn()
			if lastErr == nil {
				return result, nil
			}

			// Only retry on transient errors
			if !isRetryableError(lastErr) {
				return result, lastErr
			}
		}

		return result, fmt.Errorf("max retries (%d) exceeded: %w", conf.maxRetries, lastErr)
	}
}

// concurrentUnitsWorker returns a function that processes units concurrently
// using a pool of workers. The processUnit function is called for each unit.
// If failFast is true, processing stops on the first error.
// If failFast is false, all errors are collected and returned joined.
//
// clientsReady streams clients as each finishes provisioning, so workers begin
// draining the queue the moment their client is ready rather than waiting for
// every client to provision. The channel must be closed once no more clients
// will arrive. Each client contributes `capacity` worker goroutines that pull
// from the shared jobs queue, so per-client concurrency never exceeds that
// server's limit and work naturally flows to whichever server has a free worker.
//
// workers, when > 0, is a global concurrency cap enforced by a shared semaphore:
// at most `workers` units run at once across all servers. When 0 there is no
// extra cap and concurrency is just the sum of the servers' capacities. A worker
// only takes a semaphore slot once it already holds a unit on its client (the
// client space is acquired first), so a scarce global slot is never parked while
// waiting for a free server.
//
// ctx/cancel are shared with client provisioning: cancel is called once all
// units are processed (or on failFast), which aborts still-in-flight queries,
// retries, slot-waits, and status fetches so this returns without waiting for
// them. The caller owns ctx and must not cancel it until this function returns.
func concurrentUnitsWorker[Unit any, Result any](
	ctx context.Context,
	cancel context.CancelFunc,
	clientsReady <-chan clientWorkers,
	processUnit func(client namedClient, unit Unit) (Result, error),
	failFast bool,
	workers int,
) func(units ...Unit) ([]Result, error) {
	type resultOrError struct {
		result Result
		err    error
	}

	return func(units ...Unit) ([]Result, error) {
		// No work: don't wait on any client. Cancel so in-flight provisioning
		// unwinds; the caller waits on provisioning before touching shared state.
		if len(units) == 0 {
			cancel()
			return nil, nil
		}

		// The full set of work is known up front, so pre-fill and close the
		// queue. Workers that attach later simply drain the buffered channel.
		jobs := make(chan Unit, len(units))
		for _, unit := range units {
			jobs <- unit
		}
		close(jobs)

		// Global concurrency cap. nil when uncapped (workers == 0), in which
		// case the sum of client capacities is the only limit.
		var sem chan struct{}
		if workers > 0 {
			sem = make(chan struct{}, workers)
		}

		results := make(chan resultOrError)

		// The launcher starts each client's workers as the client becomes
		// ready. launcherDone closes once clientsReady is drained, guaranteeing
		// every workerWg.Add has happened before the closer calls Wait.
		var workerWg sync.WaitGroup
		launcherDone := make(chan struct{})
		go func() {
			for cw := range clientsReady {
				for i := 0; i < cw.capacity; i++ {
					workerWg.Add(1)
					go func(c namedClient) {
						defer workerWg.Done()
						for unit := range jobs {
							select {
							case <-ctx.Done():
								return
							default:
							}
							// Client space is already held (this goroutine);
							// now take a global slot before doing the work.
							if sem != nil {
								select {
								case <-ctx.Done():
									return
								case sem <- struct{}{}:
								}
							}
							result, err := processUnit(c, unit)
							if sem != nil {
								<-sem
							}
							select {
							case <-ctx.Done():
								return
							case results <- resultOrError{result, err}:
							}
						}
					}(cw.client)
				}
			}
			close(launcherDone)
		}()

		go func() {
			<-launcherDone
			workerWg.Wait()
			close(results)
		}()

		var allResults []Result
		var errs []error
		var firstErr error
		received := 0

		for r := range results {
			received++
			if r.err != nil {
				if !failFast {
					errs = append(errs, r.err)
				} else if firstErr == nil {
					firstErr = r.err
					cancel()
					// Continue draining to allow workers to finish
				}
			} else {
				allResults = append(allResults, r.result)
			}
			// Once every unit has a result, no client is still needed. Cancel so
			// a slow server's in-flight status fetch aborts rather than blocking
			// our return; without this, results stays open until every
			// provisioning attempt (including the slow one) completes.
			if received == len(units) {
				log.Printf("All %d units processed; cancelling any in-flight provisioning and shutting down", len(units))
				cancel()
			}
		}

		if firstErr != nil {
			return allResults, firstErr
		}
		if received < len(units) {
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
			} else {
				errs = append(errs, fmt.Errorf("no overpass servers available"))
			}
		}
		return allResults, errors.Join(errs...)
	}
}

// resolveWorkResult separates query elements and resolves geometry against the split.
func resolveWorkResult(unit workUnit, elements []element) workResult {
	var nodeElements []element
	var wps []wayPoint
	for _, e := range elements {
		switch e.Type {
		case "node":
			nodeElements = append(nodeElements, e)
		case "way":
			wps = append(wps, processWayElement(e, unit.routePoints)...)
		case "relation":
			wps = append(wps, processRelationElement(e, unit.routePoints)...)
		}
	}

	log.Printf("Split %d: %d nodes, %d way points", unit.splitIndex+1, len(nodeElements), len(wps))

	return workResult{
		splitIndex: unit.splitIndex,
		nodes:      nodeElements,
		wayPoints:  wps,
	}
}

// queryCache reports misses explicitly, including expired entries. A hit may
// contain zero elements. Store saves the raw response and returns its elements.
type queryCache interface {
	Lookup(renderedQuery string) ([]element, bool, error)
	Store(renderedQuery string, body io.Reader) ([]element, error)
}

type preparedQuery struct {
	unit  workUnit
	query string
}

// queryOrchestrator prepares route queries and resolves cache hits locally.
// Only prepared cache misses enter the client worker pool. startClients starts
// provisioning on demand and streams registrations as servers become ready;
// its cleanup function must be called after cancelling the supplied context.
type queryOrchestrator struct {
	cache        queryCache
	queryTimeout time.Duration
	retry        retryConfig
	failFast     bool
	workers      int
	startClients func(context.Context) (<-chan clientWorkers, func())
}

func (o queryOrchestrator) Run(ctx context.Context, units []workUnit) ([]workResult, error) {
	var results []workResult
	var pending []preparedQuery
	var errs []error
	for _, unit := range units {
		if err := ctx.Err(); err != nil {
			return results, errors.Join(append(errs, err)...)
		}
		renderedQuery, err := renderUnionQuery(unit.queries, unit.routePoints, o.queryTimeout)
		if err != nil {
			err = fmt.Errorf("split %d: rendering union query: %w", unit.splitIndex+1, err)
		} else {
			var elements []element
			var hit bool
			elements, hit, err = o.cache.Lookup(renderedQuery)
			if err != nil {
				err = fmt.Errorf("split %d: reading cache: %w", unit.splitIndex+1, err)
			} else if hit {
				results = append(results, resolveWorkResult(unit, elements))
			} else {
				pending = append(pending, preparedQuery{unit: unit, query: renderedQuery})
			}
		}
		if err != nil {
			if o.failFast {
				return results, err
			}
			errs = append(errs, err)
		}
	}
	if len(pending) == 0 {
		return results, errors.Join(errs...)
	}

	poolCtx, cancel := context.WithCancel(ctx)
	clientsReady, closeClients := o.startClients(poolCtx)
	defer func() {
		cancel()
		closeClients()
	}()
	queryWithRetry := retrier[[]element](o.retry)
	process := concurrentUnitsWorker(poolCtx, cancel, clientsReady, func(c namedClient, job preparedQuery) (workResult, error) {
		log.Printf("Worker [%s] processing split %d", c.name, job.unit.splitIndex+1)
		elements, err := queryWithRetry(poolCtx, func() ([]element, error) {
			return fetchQueryElements(poolCtx, o.cache, c.client.Query, job.query)
		})
		if err != nil {
			return workResult{}, fmt.Errorf("split %d [%s]: querying elements: %w", job.unit.splitIndex+1, c.name, err)
		}
		return resolveWorkResult(job.unit, elements), nil
	}, o.failFast, o.workers)
	queriedResults, err := process(pending...)
	results = append(results, queriedResults...)
	if err != nil {
		errs = append(errs, err)
	}
	return results, errors.Join(errs...)
}

// fetchQueryElements executes a prepared cache miss and stores the response.
// Cache lookup and query rendering belong to the orchestrator.
func fetchQueryElements(
	ctx context.Context,
	cache queryCache,
	makeQueryRequest func(context.Context, string) (*http.Response, error),
	renderedQuery string,
) ([]element, error) {
	resp, err := makeQueryRequest(ctx, renderedQuery)
	if err != nil {
		return nil, fmt.Errorf("posting query: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() // Preserve the HTTP error that controls retries.
		return nil, &httpStatusError{statusCode: resp.StatusCode, status: resp.Status}
	}
	elements, err := cache.Store(renderedQuery, resp.Body)
	if err != nil {
		_ = resp.Body.Close() // Preserve the cache write or response decoding error.
		return nil, err
	}
	if err := resp.Body.Close(); err != nil {
		return nil, fmt.Errorf("closing response body: %w", err)
	}
	return elements, nil
}

// provisionClients registers each server with its capacity as soon as Start
// succeeds. A slow server never holds up workers registered by another server.
// The caller cancels ctx before invoking cleanup, which waits for provisioning
// to finish and closes every client that successfully started.
func provisionClients(ctx context.Context, endpoints []endpointSpec, timeout time.Duration) (<-chan clientWorkers, func()) {
	clientsReady := make(chan clientWorkers, len(endpoints))
	var readyMu sync.Mutex
	var readyClients []namedClient
	var provisionWg sync.WaitGroup
	for _, ep := range endpoints {
		provisionWg.Add(1)
		go func(ep endpointSpec) {
			defer provisionWg.Done()
			c := overpass.NewClient(ep.Interpreter, ep.Status, timeout)
			if err := c.Start(ctx); err != nil {
				c.Close()
				if errors.Is(err, context.Canceled) {
					log.Printf("Overpass server %q provisioning cancelled (no longer needed)", ep.Name)
				} else {
					log.Printf("WARN: starting overpass client %q: %v (skipping)", ep.Name, err)
				}
				return
			}

			capacity := c.RateLimit()
			if c.Unlimited() {
				capacity = ep.Concurrency
				if capacity <= 0 {
					capacity = defaultUnlimitedConcurrency
				}
				log.Printf("Overpass server %q ready: unlimited (using concurrency=%d)", ep.Name, capacity)
			} else {
				log.Printf("Overpass server %q ready: rate limit=%d", ep.Name, capacity)
			}

			nc := namedClient{name: ep.Name, client: c}
			readyMu.Lock()
			readyClients = append(readyClients, nc)
			readyMu.Unlock()
			select {
			case clientsReady <- clientWorkers{client: nc, capacity: capacity}:
				log.Printf("Overpass server %q joining pool with capacity %d", ep.Name, capacity)
			case <-ctx.Done():
				log.Printf("Overpass server %q ready but no longer needed", ep.Name)
			}
		}(ep)
	}
	go func() {
		provisionWg.Wait()
		close(clientsReady)
	}()
	return clientsReady, func() {
		provisionWg.Wait()
		for _, nc := range readyClients {
			nc.client.Close()
		}
	}
}
