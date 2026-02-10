package overpass

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client is a rate-limited Overpass API client that uses the /api/status endpoint
// to proactively manage slot availability.
type Client struct {
	interpreterEndpoint string
	httpClient          *http.Client
	fetchStatus         func() (Status, error)

	tokens    chan struct{}    // buffered channel, cap = rate limit
	requests  chan slotRequest // incoming slot requests
	done      chan struct{}    // shutdown signal
	rateLimit int              // cached from initial status fetch

	startOnce sync.Once
	closeOnce sync.Once
}

// slotRequest represents a request for an API slot
type slotRequest struct {
	ctx    context.Context
	result chan error
}

// NewClient creates a new rate-limited Overpass client.
// Call Start() before using Query().
func NewClient(interpreterEndpoint, statusEndpoint string) *Client {
	return &Client{
		interpreterEndpoint: interpreterEndpoint,
		httpClient:          &http.Client{Timeout: 120 * time.Second},
		fetchStatus:         StatusFetcher(statusEndpoint),
		requests:            make(chan slotRequest),
		done:                make(chan struct{}),
	}
}

// Start initializes the client by fetching the initial status and starting
// the coordinator goroutine. It must be called before Query().
func (c *Client) Start(ctx context.Context) error {
	var err error
	c.startOnce.Do(func() {
		status, fetchErr := c.fetchStatus()
		if fetchErr != nil {
			err = fmt.Errorf("initial status fetch: %w", fetchErr)
			return
		}

		if status.RateLimit < 1 {
			err = fmt.Errorf("invalid rate limit from status: %d", status.RateLimit)
			return
		}

		c.rateLimit = status.RateLimit
		c.tokens = make(chan struct{}, status.RateLimit)

		// Populate initial tokens - pending slots will be handled by coordinator
		// when requests actually need to wait
		for i := 0; i < status.AvailableNow && i < status.RateLimit; i++ {
			c.tokens <- struct{}{}
		}

		go c.coordinator(ctx)

		if len(status.NextSlotWaits) > 0 {
			log.Printf("Overpass client started: rate limit=%d, available now=%d, next slot in %s",
				status.RateLimit, status.AvailableNow, status.NextSlotWaits[0].Round(time.Second))
		} else {
			log.Printf("Overpass client started: rate limit=%d, available now=%d",
				status.RateLimit, status.AvailableNow)
		}
	})
	return err
}

// RateLimit returns the cached rate limit from the initial status fetch.
func (c *Client) RateLimit() int {
	return c.rateLimit
}

// Close shuts down the client and cancels any pending requests.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
	})
}

// Query executes a query against the Overpass interpreter endpoint.
// It blocks until an API slot is available.
func (c *Client) Query(ctx context.Context, query string) (*http.Response, error) {
	// Request a slot
	result := make(chan error, 1)
	select {
	case c.requests <- slotRequest{ctx: ctx, result: result}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("client closed")
	}

	// Wait for slot
	select {
	case err := <-result:
		if err != nil {
			return nil, fmt.Errorf("waiting for API slot: %w", err)
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("client closed")
	}

	// Make the actual request
	req, err := http.NewRequestWithContext(ctx, "POST", c.interpreterEndpoint, strings.NewReader(query))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	// Overpass API usage policy expects clients to identify themselves.
	// Requests without User-Agent may be deprioritised by the server.
	req.Header.Set("User-Agent", "route-poi-finder")

	return c.httpClient.Do(req)
}

// coordinator manages slot allocation and status fetching
func (c *Client) coordinator(ctx context.Context) {
	var pendingRequests []slotRequest
	timerFired := make(chan struct{}, 1)
	var timerActive bool
	var nextSlotWait time.Duration

	for {
		select {
		case req := <-c.requests:
			// Check if context already cancelled
			select {
			case <-req.ctx.Done():
				req.result <- req.ctx.Err()
				continue
			default:
			}

			// Try to get token immediately
			select {
			case <-c.tokens:
				req.result <- nil // got slot
			default:
				// No token available, queue the request
				pendingRequests = append(pendingRequests, req)
				if timerActive {
					log.Printf("Request queued, %d pending, timer active (wait: %s)",
						len(pendingRequests), nextSlotWait.Round(time.Second))
				} else {
					log.Printf("Request queued, %d pending, no timer", len(pendingRequests))
				}

				// If no timer running, fetch status now
				if !timerActive {
					pendingRequests, timerActive, nextSlotWait = c.fetchStatusAndSchedule(
						ctx, pendingRequests, timerFired)
				}
			}

		case <-timerFired:
			timerActive = false
			// Timer fired - fetch fresh status and process
			if len(pendingRequests) > 0 {
				pendingRequests, timerActive, nextSlotWait = c.fetchStatusAndSchedule(
					ctx, pendingRequests, timerFired)
			}

		case <-ctx.Done():
			// Cancel all pending requests
			for _, req := range pendingRequests {
				req.result <- ctx.Err()
			}
			return

		case <-c.done:
			// Cancel all pending requests
			for _, req := range pendingRequests {
				req.result <- errors.New("client closed")
			}
			return
		}
	}
}

// fetchStatusAndSchedule fetches status, serves what it can, and schedules one timer if needed
func (c *Client) fetchStatusAndSchedule(
	ctx context.Context,
	pendingRequests []slotRequest,
	timerFired chan<- struct{},
) (remaining []slotRequest, timerActive bool, nextWait time.Duration) {
	status, err := c.fetchStatus()
	if err != nil {
		// Fail the oldest pending request
		if len(pendingRequests) > 0 {
			pendingRequests[0].result <- fmt.Errorf("fetching API status: %w", err)
			pendingRequests = pendingRequests[1:]
		}
		return pendingRequests, false, 0
	}

	// Edge case: no slots and no wait times
	if status.AvailableNow == 0 && len(status.NextSlotWaits) == 0 {
		if len(pendingRequests) > 0 {
			pendingRequests[0].result <- errors.New("no slots available and no wait times provided")
			pendingRequests = pendingRequests[1:]
		}
		return pendingRequests, false, 0
	}

	if len(status.NextSlotWaits) > 0 {
		log.Printf("Status: %d available now, next slot in %s",
			status.AvailableNow, status.NextSlotWaits[0].Round(time.Second))
	} else {
		log.Printf("Status: %d available now", status.AvailableNow)
	}

	// Drain any stale tokens (fresh status = fresh truth)
	for {
		select {
		case <-c.tokens:
		default:
			goto drained
		}
	}
drained:

	// Add tokens for immediately available slots
	for i := 0; i < status.AvailableNow; i++ {
		select {
		case c.tokens <- struct{}{}:
		default:
			break // buffer full
		}
	}

	// Serve pending requests with available tokens
	pendingRequests = c.servePendingRequests(pendingRequests)

	// If still have pending requests and there's a wait time, schedule timer
	if len(pendingRequests) > 0 && len(status.NextSlotWaits) > 0 {
		nextWait = status.NextSlotWaits[0]
		time.AfterFunc(nextWait, func() {
			select {
			case timerFired <- struct{}{}:
			default:
			}
		})
		return pendingRequests, true, nextWait
	}

	return pendingRequests, false, 0
}

// servePendingRequests attempts to serve pending requests with available tokens
func (c *Client) servePendingRequests(pending []slotRequest) []slotRequest {
	var remaining []slotRequest
	for _, req := range pending {
		// Check if context cancelled
		select {
		case <-req.ctx.Done():
			req.result <- req.ctx.Err()
			continue
		default:
		}

		// Try to get a token
		select {
		case <-c.tokens:
			req.result <- nil
		default:
			remaining = append(remaining, req)
		}
	}
	return remaining
}
