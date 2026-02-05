# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run Commands

```bash
# Build
go build -o route-poi-finder

# Run directly
go run main.go [flags] <gpx-file>

# Example with flags
go run main.go --name-prefix "Day1-" --split 10 route.gpx
```

No tests exist currently. When added:
```bash
go test ./...              # All tests
go test -run TestName -v   # Single test
```

## Architecture

Single-file Go CLI tool (`main.go`) that finds Points of Interest along GPS routes.

**Processing Flow:**
1. Parse GPX file (requires exactly one track with one segment)
2. Split route into N segments (configurable via `--split`, default 5)
3. For each segment, query Overpass API for ~12 POI categories (amenities, water, tourism, shops, etc.)
4. Cache API responses in `/tmp/route-poi-finder-state/` by SHA1 hash of query
5. Extract nodes and calculate way centroids
6. Output JSON to temp directory with POIs and tag frequency statistics

**Key flags:**
- `--split` - Number of route segments for API queries (default 5)
- `--name-prefix` - Prefix for all POI names in output
- `--workers` - Number of concurrent workers for API requests (default 3)
- `--retries` - Number of retries per API request on transient failures (default 5)
- `--fail-fast` - Stop processing on first API error (default true, use `--fail-fast=false` to collect all errors)
