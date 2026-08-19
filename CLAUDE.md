# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run Commands

```bash
# Build
go build -o route-poi-finder .

# Run directly (package main spans several files, so `go run main.go` fails)
go run . [flags] <gpx-file>

# Example with flags (output is GeoJSON on stdout)
go run . --name-prefix "Day1-" --split 10 route.gpx > pois.geojson
```

Tests live in `main_test.go`:
```bash
go test ./...              # All tests
go test -run TestName -v   # Single test
```

## Architecture

Go CLI tool that finds Points of Interest along GPS routes. `package main` is split across
`main.go` (everything but the query definitions) and `queries.go` (the POI query definitions);
the Overpass HTTP client, with per-server rate-limit slot handling, lives in `overpass/`.

**Processing Flow:**
1. Parse GPX file (requires exactly one track with one segment, containing at least one point)
2. Split route points into chunks of `len(points)/--split` (default 5). Integer division means a
   remainder leaves one extra short chunk, so `--split 5` normally yields 6 chunks, not 5
3. For each chunk, issue **one** consolidated Overpass union query covering all 18 category
   definitions in `queries.go` (amenities, water, tourism, shops, etc.), each with its own
   `around:` radius (500/1000/2000m, or 80m when unset). So query count tracks chunks, not categories
4. Cache API responses in `--cache-dir` (default `$HOME/.route-poi-finder-state`, so the cache
   persists across reboots), keyed by base64url-encoded SHA1 of the rendered query. Entries older
   than `--cache-ttl` are re-queried; younger ones are replayed from disk
5. Extract nodes; for ways/relations, use the points where their geometry crosses the route,
   falling back to the closest approach point when there is no crossing
6. Write a GeoJSON `FeatureCollection` (Point features with `name`/`category`/`categories`/
   `osm_type`/`osmid`/`tags` properties) to stdout by default. Tag frequency statistics are only
   logged to stderr, not included in the output

**Key flags:**
- `--split` - Number of route chunks for API queries (default 5, see caveat above)
- `--name-prefix` - Prefix for all POI names in output
- `--out` - Output destination (default `-` = stdout; a path writes/truncates that file; empty string writes to a temp file)
- `--cache-dir` - Cache directory (default `$HOME/.route-poi-finder-state`, or `.route-poi-finder-state` if the home dir can't be determined)
- `--cache-ttl` - Max age of cached responses before re-querying (default `672h`, i.e. 28 days). **Trap:** a rerun meant to pick up new OSM data silently replays cached responses and produces identical output; use `--cache-ttl 0` to force fresh queries without deleting the cache
- `--workers` - Global cap on concurrent API requests across all servers (default 0=no cap; concurrency is then the sum of the servers' rate limits)
- `--retries` - Number of retries per API request on transient failures (default 5)
- `--fail-fast` - Stop processing on first API error (default true, use `--fail-fast=false` to collect all errors)
- `--overpass-endpoint` - Repeatable `NAME=INTERPRETER_URL,STATUS_URL[,CONCURRENCY]`; defaults to overpass-api.de and overpass.private.coffee. `CONCURRENCY` only applies to servers reporting an unlimited rate

## Task tracking & commits

- `todos.md` tracks only *open* work. When a task is finished, **remove** its entry — do not leave it marked "done". The durable record of what changed and why lives in the commit that did it, not in `todos.md`.
- Commit messages should describe both the **what** and the **why** of the change (in addition to the gitmoji and no-Claude-co-author rules in the global config).
