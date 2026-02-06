package overpass

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Status holds parsed status from the Overpass API status endpoint
type Status struct {
	RateLimit    int
	AvailableNow int
}

// StatusFetcher returns a function that fetches and parses the current status from the Overpass API
func StatusFetcher(endpoint string) func() (Status, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	return func() (Status, error) {
		resp, err := client.Get(endpoint)
		if err != nil {
			return Status{}, fmt.Errorf("fetching status: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return Status{}, fmt.Errorf("status endpoint returned %d", resp.StatusCode)
		}

		return parseStatusResponse(resp.Body)
	}
}

// parseStatusResponse parses the text response from /api/status endpoint
func parseStatusResponse(r io.Reader) (Status, error) {
	var status Status
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Parse "Rate limit: N"
		if strings.HasPrefix(line, "Rate limit:") {
			parts := strings.TrimPrefix(line, "Rate limit:")
			n, err := strconv.Atoi(strings.TrimSpace(parts))
			if err == nil {
				status.RateLimit = n
			}
			continue
		}

		// Parse "N slots available now."
		if strings.HasSuffix(line, "slots available now.") {
			parts := strings.TrimSuffix(line, "slots available now.")
			n, err := strconv.Atoi(strings.TrimSpace(parts))
			if err == nil {
				status.AvailableNow = n
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Status{}, fmt.Errorf("scanning status response: %w", err)
	}

	return status, nil
}
