# Overpass API Usage: Audit Findings & TODOs

## Use `out center` instead of `(._;>;); out body qt;` for way queries

**File:** `main.go:892-894` (query suffix), `main.go:921-967` (`wayCentres` function)
**Type:** Efficiency
**Effort:** Medium

The current approach for way queries uses the recurse-down pattern `(._;>;); out body qt;`, which fetches the matched ways *and every constituent node* of those ways. The `wayCentres()` function then manually computes a centroid by averaging node coordinates. This is expensive in terms of data transfer and server processing.

The Overpass API provides `out center` specifically for this use case. It returns ways with a single representative coordinate (the center of the way's bounding box) without needing to recurse into constituent nodes. This is the standard approach for POI-style queries and is what tools like Overpass Turbo use.

The trade-off is that `out center` returns the bounding box center rather than a coordinate average. For POI placement (the purpose of this tool), the difference is negligible.

Switching to `out center` would:
- Eliminate the `(._;>;)` recursion (the most expensive part of the query)
- Dramatically reduce response sizes (no node elements returned for ways)
- Remove the need for the `wayCentres()` manual centroid calculation
- Reduce server-side processing time and memory usage

**References:**
- Overpass QL output formats: https://wiki.openstreetmap.org/wiki/Overpass_API/Overpass_QL#Output_Format_(out)
- Overpass documentation on output modes and geometry: https://dev.overpass-api.de/overpass-doc/en/full_data/osm_types.html
- OSM Help discussion on `out center`: https://help.openstreetmap.org/questions/60042/efficient-approach-to-get-center-coordinate-for-ways-via-overpass/

---

## Combine node and way queries into a single union query

**File:** `main.go:492-506` (`unitProcessor`), `main.go:900-967` (`nodes` and `wayCentres` functions)
**Type:** Efficiency
**Effort:** Medium

Each work unit currently makes **2 separate API calls** per category per split: one querying `node[...]` and one querying `way[...]`. With the default `--split 5` and 16 query categories, this totals **160 API requests**. The Overpass documentation recommends building larger consolidated queries rather than many small ones, as the server can optimise better when it sees the full picture.

These two queries can be combined into a single union query:

```
[out:json];
(
  node[amenity~"^(cafe|pub)$"](around:1000,...);
  way[amenity~"^(cafe|pub)$"](around:1000,...);
);
out center;
```

With `out center`, both nodes (which return their own lat/lon) and ways (which return their bounding box center) produce a uniform output format. This halves the total request count from `2 * splits * 16` to `splits * 16` (160 to 80 with defaults).

**References:**
- Overpass QL union statements: https://wiki.openstreetmap.org/wiki/Overpass_API/Overpass_QL#Union
- Overpass Language Guide on combining queries: https://wiki.openstreetmap.org/wiki/Overpass_API/Language_Guide
- Overpass documentation on output with center: https://dev.overpass-api.de/overpass-doc/en/full_data/osm_types.html

---

## Downsample route points in `around` filter

**File:** `main.go:861-897` (`queryRouteComponent`)
**Type:** Efficiency
**Effort:** Medium

`queryRouteComponent` emits every single GPX point in the segment into the `around:` filter. A GPS track typically records a point every 1-5 seconds. On a long route with `--split 5`, each segment could contain hundreds or thousands of coordinate pairs, producing very large query strings.

The Overpass docs note that the `around` filter performs spherical distance calculations for each coordinate pair and recommend using bounding boxes "when possible" for performance. Since this tool uses search radii of 200-2000m, GPS-level point density is unnecessary. Downsampling to one point every ~100-200m (e.g. via Ramer-Douglas-Peucker simplification or simple distance-based thinning) would produce nearly identical spatial coverage while significantly reducing:
- Query string size
- Server-side distance computation
- Memory usage counted against the `maxsize` limit

**References:**
- Overpass blog on around performance: https://dev.overpass-api.de/blog/around_performance.html
- Overpass QL around filter: https://wiki.openstreetmap.org/wiki/Overpass_API/Overpass_QL#By_radius_around_a_linestring_(around)
- Overpass documentation on polygon and around: https://dev.overpass-api.de/overpass-doc/en/full_data/polygon.html
- Ramer-Douglas-Peucker algorithm: https://en.wikipedia.org/wiki/Ramer%E2%80%93Douglas%E2%80%93Peucker_algorithm

---

## Add cache TTL or invalidation mechanism

**File:** `main.go:1032-1036`
**Type:** Data freshness
**Effort:** Small

Cached API responses persist indefinitely with no expiration. OSM data is continuously updated by contributors: new POIs are added, old ones are removed, tags are corrected. A query cached months ago may return stale data (e.g. a closed restaurant still appearing, a new water source missing).

If a cached item is over 28 days old, it should be replaced. Check file modification time and re-query if the cache entry exceeds this threshold. A `--no-cache` flag to bypass the cache entirely would also help during iterative use.

**References:**
- Go os.FileInfo ModTime: https://pkg.go.dev/os#FileInfo
- OSM data update frequency: https://wiki.openstreetmap.org/wiki/Planet.osm#Update_frequency

---

## Make Overpass API endpoint configurable

**File:** `overpass/client.go` (endpoint construction)
**Type:** Resilience
**Effort:** Small

The API endpoint `https://overpass-api.de/api/interpreter` is hardcoded. If this server is down, under maintenance, or rate-limiting aggressively, the tool is unusable. Several alternative public Overpass instances exist, and users may also run their own.

A `--overpass-url` flag would allow pointing to alternatives:
- `https://overpass.kumi.systems/api/interpreter`
- `https://maps.mail.ru/osm/tools/overpass/api/interpreter`
- Self-hosted instances

**References:**
- Public Overpass API instances: https://wiki.openstreetmap.org/wiki/Overpass_API#Public_Overpass_API_instances
- Self-hosting Overpass: https://wiki.openstreetmap.org/wiki/Overpass_API/Installation

---

## Consider boundary/area query limitations with `around`

**File:** `main.go:147-160` (boundary query category)
**Type:** Design consideration
**Effort:** Medium

Queries for `boundary=national_park` and similar area features use the default 80m `around` radius. The `around` filter on ways checks proximity to the way's *geometry* (the boundary line), not whether the route is *inside* the area. This means a route travelling through the middle of a large national park will not match if it's more than 80m from the park boundary line.

This is a fundamental limitation of the `around` approach for area features. Possible mitigations:
- Increase the radius for boundary queries (but this creates false positives for routes near but outside parks)
- Use an `is_in` query to find areas containing route points (different Overpass query pattern)
- Accept the limitation and document it

**References:**
- Overpass QL `is_in` filter: https://wiki.openstreetmap.org/wiki/Overpass_API/Overpass_QL#By_area_(is_in)
- Overpass QL around filter on ways: https://wiki.openstreetmap.org/wiki/Overpass_API/Overpass_QL#By_radius_around_a_linestring_(around)

---

## Review `drinking_water=yes` query breadth

**File:** `main.go:170-176` (drinking water tag query)
**Type:** Data quality
**Effort:** Small

The query for `drinking_water=yes` uses a 2000m radius. This tag appears on many features that are not standalone water sources: restaurants, public buildings, petrol stations, and other amenities may have `drinking_water=yes` to indicate that drinking water is available on-premises. A 2km buffer on a long route can return many results that are not useful as dedicated water refill points.

Consider either:
- Tightening the radius
- Adding negative conditions to exclude certain feature types (e.g. `[amenity!="restaurant"]`)
- Combining with other tags to narrow results (e.g. require `amenity=drinking_water` OR `man_made=water_tap` in addition to the tag)

**References:**
- OSM wiki for `drinking_water=yes`: https://wiki.openstreetmap.org/wiki/Key:drinking_water
- OSM Taginfo for `drinking_water=yes`: https://taginfo.openstreetmap.org/tags/drinking_water=yes

---

## `mountain_range` is typically tagged on relations, not nodes/ways

**File:** `main.go:138`
**Type:** Data coverage
**Effort:** Small

The value `mountain_range` in the `natural` tag query is unlikely to return meaningful results. Mountain ranges in OSM are predominantly tagged as `type=mountain_range` on **relations** (which group multiple peaks, ridges, etc.). The tool only queries `node` and `way` types, so these relations will not be found.

Querying relations is generally discouraged by the Overpass documentation due to the risk of pulling in enormous hierarchical data. If mountain range information is desired, consider querying for `natural=ridge` and `natural=peak` (already included) as proxies, and removing `mountain_range` to avoid silent no-ops.

**References:**
- OSM wiki for mountain ranges: https://wiki.openstreetmap.org/wiki/Tag:natural%3Dmountain_range
- Overpass documentation discouraging relation-of-relation queries: https://wiki.openstreetmap.org/wiki/Overpass_API/Language_Guide

---

## Status fetch failure drops pending requests without retry

**File:** `overpass/client.go:202-209`
**Type:** Robustness
**Effort:** Small

When `fetchStatusAndSchedule` fails to fetch the `/api/status` endpoint (network error, temporary server issue), it fails the **oldest pending request** and returns with no timer scheduled. Any remaining pending requests sit in the queue indefinitely until a new incoming request triggers another status fetch attempt. There is no retry with backoff for the status fetch itself.

If the status endpoint is temporarily unreachable (e.g. brief network blip), this causes one request to fail unnecessarily and leaves remaining queued requests stranded until new activity arrives. A retry with a short backoff (e.g. 5s, 10s, 20s) before failing requests would be more resilient.

**References:**
- Overpass API status endpoint: https://wiki.openstreetmap.org/wiki/Overpass_API#Status
- Go time.AfterFunc for scheduling retries: https://pkg.go.dev/time#AfterFunc

---

## Slots are not returned after query completion

**File:** `overpass/client.go:100-130`
**Type:** Design observation
**Effort:** N/A (by design)

The rate-limiting client consumes a token from the channel before making a query (`client.go:152` or `client.go:279`) but never puts it back after the query completes. This means after all initial tokens are consumed, every subsequent request must go through the status-fetch path to discover newly available slots.

This is likely intentional since the Overpass API's slot system has cooldown periods (a slot is occupied for execution time plus a server-determined cooldown), so a token is not immediately reusable. The coordinator correctly discovers slot availability by polling `/api/status`. However, this means the client always relies on status fetches for slot replenishment, adding latency to every request after the initial burst. If the status endpoint is slow, this becomes a bottleneck.

An alternative design would return tokens after a conservative cooldown estimate, falling back to status polling only when tokens are exhausted.

**References:**
- Overpass API slot and cooldown system: https://dev.overpass-api.de/overpass-doc/en/preface/commons.html
- Overpass API wiki on rate limiting: https://wiki.openstreetmap.org/wiki/Overpass_API#Rate_Limiting

---

## Name resolution ignores localised and alternative name tags

**File:** `main.go:1084-1103` (`resolveName`)
**Type:** Data quality
**Effort:** Small

`resolveName` checks a fixed list of 10 tag keys (`name`, `amenity`, `tourism`, etc.) in priority order. It does not consider localised name tags (`name:en`, `name:de`, `name:fr`, etc.), alternative names (`alt_name`), international names (`int_name`), or official names (`official_name`).

For routes in non-English-speaking regions, the `name` tag is often in the local script (e.g. Cyrillic, CJK, Arabic). If the user would benefit from English names, checking `name:en` before or after `name` would improve readability. Similarly, `alt_name` can provide useful context when `name` is missing or unhelpful.

**References:**
- OSM wiki on multilingual names: https://wiki.openstreetmap.org/wiki/Multilingual_names
- OSM wiki on name key: https://wiki.openstreetmap.org/wiki/Key:name
- OSM wiki on `alt_name`: https://wiki.openstreetmap.org/wiki/Key:alt_name

---

## Waterway query excludes `stream` which may be useful

**File:** `main.go:185-195`
**Type:** Data coverage
**Effort:** Trivial

The waterway query uses a NOT filter to exclude `drain`, `dam`, `stream`, `ditch`, and `canal`. Excluding `drain`, `ditch`, and `canal` makes sense as these are typically man-made and uninteresting for route travellers. However, `stream` is a natural waterway that is often relevant for outdoor routes (as a water source, swimming spot, or crossing point). Its exclusion may be intentional (to reduce noise from the many mapped streams) but is worth reconsidering, especially since `river` is kept and streams are arguably more accessible.

If stream volume is a concern, consider keeping `stream` but reducing the search radius for the waterway category.

**References:**
- OSM wiki for `waterway=stream`: https://wiki.openstreetmap.org/wiki/Tag:waterway%3Dstream
- OSM wiki for `waterway=river`: https://wiki.openstreetmap.org/wiki/Tag:waterway%3Driver
- OSM Taginfo for waterway values: https://taginfo.openstreetmap.org/keys/waterway#values

---

## Large area features produce misleading centroid points

**File:** `main.go:921-967` (`wayCentres`), `main.go:147-160` (boundary queries)
**Type:** Design observation
**Effort:** Medium

When a query matches large area features (national parks, nature reserves, large rivers), the centroid (or bounding box center with `out center`) may be far from the actual route. For example, a national park boundary way spanning 50km would produce a single POI point at its geographic center, which could be 25km from the route.

This is inherent to reducing areas to points. For the tool's purpose of finding POIs along a route, a more useful approach for area features might be to:
- Report the nearest point on the way's geometry to the route (requires `out geom`)
- Simply report the feature's name and tags without a coordinate, as contextual information
- Use `is_in` to find enclosing areas, which avoids the centroid problem entirely

**References:**
- Overpass QL `is_in` for area containment: https://wiki.openstreetmap.org/wiki/Overpass_API/Overpass_QL#By_area_(is_in)
- Overpass QL `out geom` for full geometry: https://wiki.openstreetmap.org/wiki/Overpass_API/Overpass_QL#Output_Format_(out)

---

## Concurrent cache writes are not protected

**File:** `main.go:1037-1068`
**Type:** Robustness
**Effort:** Small

If two workers happen to process the same query hash concurrently (unlikely with the current split-based design, but possible if two categories produce identical rendered queries), both will miss the cache, both will query the API, and both will write to the same file path. The second write will overwrite the first, which is benign for correctness (both contain the same data), but the concurrent `io.Copy` calls could interleave and produce a corrupted file.

This is largely a theoretical concern given the current architecture (each work unit has a unique combination of query type, conditions, and route segment). However, if the tool evolves to support overlapping segments or shared query patterns, this could become a real issue. Using atomic writes (as suggested in the "Make cache writes atomic" section) with unique temp files would also solve this.

It would be great to intoduce a single-flight mechanism, so that if two workers try to request the same data, only the first worker to request it makes a request, and the second (or more) just waits for the result of the first query through some data joining mechanism. This may make the effort greater than "Small".

**References:**
- Go os.CreateTemp for unique temp files: https://pkg.go.dev/os#CreateTemp
- POSIX file write atomicity: https://pubs.opengroup.org/onlinepubs/9699919799/functions/write.html

---

## Consider consolidating queries across categories within the same split

**Type:** Efficiency (speculative)
**Effort:** Large

Currently each split generates 16 separate queries (one per POI category), each repeating the same `around` filter with the same coordinate list. The Overpass API supports arbitrarily complex union queries, so in theory all 16 categories for a single split could be merged into one large union:

```
[out:json];
(
  node[amenity~"^(bar|cafe|...)$"](around:1000,...);
  way[amenity~"^(bar|cafe|...)$"](around:1000,...);
  node[tourism~"^(alpine_hut|...)$"](around:200,...);
  way[tourism~"^(alpine_hut|...)$"](around:200,...);
  ...
);
out center qt;
```

This would reduce total requests from `splits * 16` to just `splits` (e.g. 80 to 5 with defaults). The server processes the spatial index lookup once for the shared `around` filter rather than 16 times. However, this would:
- Eliminate per-category caching granularity (one cache entry per split instead of per category)
- Require post-processing to separate results by category for tag statistics
- Produce much larger individual responses
- Make individual queries slower (though total time may decrease)

This is a significant architectural change and may not be worthwhile given the current caching strategy. Noted here for completeness.

**References:**
- Overpass QL union: https://wiki.openstreetmap.org/wiki/Overpass_API/Overpass_QL#Union
- Overpass blog on query consolidation: https://dev.overpass-api.de/overpass-doc/en/preface/commons.html
