package main

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/glynternet/route-poi-finder/overpass"
	gpxgo "github.com/tkrajina/gpxgo/gpx"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const (
	debug = false
)

const (
	// TODO(glynternet): workout better API
	ExistsUndefined = iota
	ExistsYes
	ExistsNo
)

type query struct {
	radius int
	// conditions are AND'd when rendered as a query
	conditions []condition
}

type condition struct {
	tag       string
	values    []string
	notValues []string
	exists    int
}

// API

type response struct {
	Elements []element `json:"elements"`
}

type element struct {
	Type     string                 `json:"type"`
	ID       int64                  `json:"id"`
	Lat      float64                `json:"lat"`
	Lon      float64                `json:"lon"`
	Nodes    []int64                `json:"nodes"`
	Tags     map[string]interface{} `json:"tags"`
	Geometry []LatLon               `json:"geometry"`
}

// MODEL

type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type wayPoint struct {
	ID   int64
	Loc  LatLon
	Tags map[string]interface{}
}

// Point is stolen from gpx project, should really import it instead
type Point struct {
	// field names matched to GPX spec
	Name        string   `json:"name"`
	Lat         float64  `json:"lat"`
	Lon         float64  `json:"lon"`
	Description string   `json:"desc"`
	Symbol      string   `json:"sym"`
	Categories  []string `json:"categories"`
	OSMID       int64    `json:"osmid"`
}

// workUnit represents a single split's worth of work for the worker pool.
// All query categories are consolidated into a single Overpass union query.
type workUnit struct {
	splitIndex  int
	queries     []query
	routePoints []gpxgo.GPXPoint
}

// workResult contains the results from processing a single split.
type workResult struct {
	splitIndex int
	nodes      []element
	wayPoints  []wayPoint
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
func concurrentUnitsWorker[Unit any, Result any](
	workerCount int,
	processUnit func(unit Unit) (Result, error),
	failFast bool,
) func(units ...Unit) ([]Result, error) {
	type resultOrError struct {
		result Result
		err    error
	}

	return func(units ...Unit) ([]Result, error) {
		jobs := make(chan Unit)
		results := make(chan resultOrError)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var wg sync.WaitGroup
		for i := 0; i < workerCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for unit := range jobs {
					select {
					case <-ctx.Done():
						return
					default:
					}
					result, err := processUnit(unit)
					select {
					case <-ctx.Done():
						return
					case results <- resultOrError{result, err}:
					}
				}
			}()
		}

		go func() {
			for _, unit := range units {
				select {
				case <-ctx.Done():
					break
				case jobs <- unit:
				}
			}
			close(jobs)
		}()

		go func() {
			wg.Wait()
			close(results)
		}()

		var allResults []Result
		var errs []error
		var firstErr error

		for r := range results {
			if r.err != nil {
				if !failFast {
					errs = append(errs, r.err)
				} else if firstErr == nil {
					firstErr = r.err
					cancel()
					// Continue draining to allow workers to finish
				}
				continue
			}
			allResults = append(allResults, r.result)
		}

		if firstErr != nil {
			return allResults, firstErr
		}
		return allResults, errors.Join(errs...)
	}
}

// unitProcessor returns a function that processes a single split by building
// a consolidated Overpass union query across all categories, executing it via
// queryResponseElementsRaw, and separating nodes from ways in the response.
func unitProcessor(
	ctx context.Context,
	cacheDir string,
	cacheTTL time.Duration,
	queryClient func(ctx context.Context, query string) (*http.Response, error),
	queryElementsWithRetry func(ctx context.Context, queryFn func() ([]element, error)) ([]element, error),
) func(unit workUnit) (workResult, error) {
	return func(unit workUnit) (workResult, error) {
		log.Printf("Worker processing split %d", unit.splitIndex+1)

		renderedQuery, err := renderUnionQuery(unit.queries, unit.routePoints)
		if err != nil {
			return workResult{}, fmt.Errorf("split %d: rendering union query: %w", unit.splitIndex+1, err)
		}

		elements, err := queryElementsWithRetry(ctx, func() ([]element, error) {
			return queryResponseElementsRaw(ctx, cacheDir, cacheTTL, queryClient, renderedQuery)
		})
		if err != nil {
			return workResult{}, fmt.Errorf("split %d: querying elements: %w", unit.splitIndex+1, err)
		}

		var nodeElements []element
		var wps []wayPoint
		for _, e := range elements {
			switch e.Type {
			case "node":
				nodeElements = append(nodeElements, e)
			case "way":
				wps = append(wps, processWayElement(e, unit.routePoints)...)
			}
		}

		log.Printf("Split %d: %d nodes, %d way points", unit.splitIndex+1, len(nodeElements), len(wps))

		return workResult{
			splitIndex: unit.splitIndex,
			nodes:      nodeElements,
			wayPoints:  wps,
		}, nil
	}
}

func main() {
	log.SetFlags(log.Ldate | log.Lmicroseconds | log.Lshortfile)
	// flags have to go before args
	// TODO(glynternet): use better flags package
	namePrefix := flag.String(`name-prefix`, ``, `prefix to place in front of all points`)
	split := flag.Uint(`split`, 5, `number of segments to split track into for querying overpass API`)
	out := flag.String(`out`, "-", `file to write output to, "-" writes to stdout`)
	workers := flag.Int(`workers`, 0, `number of concurrent workers for API requests (0=auto-detect from API rate limit)`)
	retries := flag.Int(`retries`, 5, `number of retries per API request on transient failures`)
	failFast := flag.Bool(`fail-fast`, true, `stop processing on first API error`)

	var defaultCacheDir string
	if homeDir, err := os.UserHomeDir(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Unable to determine home directory for default cache directory: %v\n", err)
		defaultCacheDir = ".route-poi-finder-state"
	} else {
		defaultCacheDir = filepath.Join(homeDir, `.route-poi-finder-state`)
	}
	cacheDir := flag.String(`cache-dir`, defaultCacheDir, `directory to cache results in`)
	cacheTTL := flag.Duration(`cache-ttl`, 28*24*time.Hour, `maximum age of cached API responses before re-querying`)
	flag.Parse()

	if *workers < 0 {
		log.Println("--workers must be at least 0")
		os.Exit(1)
	}
	if *retries < 0 {
		log.Println("--retries must be at least 0")
		os.Exit(1)
	}

	args := flag.Args()
	if len(args) != 1 {
		log.Println("must provide gpx file arg")
		os.Exit(1)
	}
	if err := mainErr(args[0], *namePrefix, *split, *workers, *retries, *failFast, *cacheDir, *cacheTTL, *out); err != nil {
		log.Println(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

// segmentIntersection tests whether segments p1-p2 and p3-p4 intersect, and
// if so returns the intersection point. It uses a standard parametric
// approach: each segment is expressed as a linear combination
//
//	S1(t) = p1 + t*(p2-p1),  t ∈ [0,1]
//	S2(u) = p3 + u*(p4-p3),  u ∈ [0,1]
//
// Setting S1(t) = S2(u) gives a 2×2 linear system solved via Cramer's rule.
// If the denominator (the cross product of the direction vectors) is zero the
// segments are parallel/collinear and we report no intersection. Otherwise
// both parameters must lie in [0,1] for the intersection to fall within both
// segments. Lat/lon are treated as Cartesian, which is adequate for the
// sub-kilometre distances involved.
func segmentIntersection(p1, p2, p3, p4 LatLon) (LatLon, bool) {
	d1Lat := p2.Lat - p1.Lat
	d1Lon := p2.Lon - p1.Lon
	d2Lat := p4.Lat - p3.Lat
	d2Lon := p4.Lon - p3.Lon

	denom := d1Lat*d2Lon - d1Lon*d2Lat
	if denom == 0 {
		return LatLon{}, false // parallel or collinear
	}

	t := ((p3.Lat-p1.Lat)*d2Lon - (p3.Lon-p1.Lon)*d2Lat) / denom
	u := ((p3.Lat-p1.Lat)*d1Lon - (p3.Lon-p1.Lon)*d1Lat) / denom

	if t < 0 || t > 1 || u < 0 || u > 1 {
		return LatLon{}, false
	}

	return LatLon{
		Lat: p1.Lat + t*d1Lat,
		Lon: p1.Lon + t*d1Lon,
	}, true
}

// closestPointOnSegment returns the nearest point on segment a-b to point p,
// along with the squared distance. It projects p onto the infinite line
// through a-b by computing t = dot(p-a, b-a) / |b-a|². Clamping t to [0,1]
// restricts the result to the segment. The projection point is then
// a + t*(b-a) and the squared Euclidean distance to p is returned to avoid
// an unnecessary sqrt.
func closestPointOnSegment(a, b, p LatLon) (LatLon, float64) {
	abLat := b.Lat - a.Lat
	abLon := b.Lon - a.Lon
	dot := (p.Lat-a.Lat)*abLat + (p.Lon-a.Lon)*abLon
	lenSq := abLat*abLat + abLon*abLon

	var t float64
	if lenSq > 0 {
		t = dot / lenSq
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
	}

	proj := LatLon{Lat: a.Lat + t*abLat, Lon: a.Lon + t*abLon}
	dLat := proj.Lat - p.Lat
	dLon := proj.Lon - p.Lon
	return proj, dLat*dLat + dLon*dLon
}

// wayRouteIntersections finds all crossing points between a way's geometry and
// the route, and also tracks the closest approach point. If crossings is
// non-empty those should be used; otherwise closest is the fallback.
func wayRouteIntersections(wayGeometry []LatLon, routePoints []gpxgo.GPXPoint) (crossings []LatLon, closest LatLon) {
	bestDistSq := math.Inf(1)
	for wi := 0; wi < len(wayGeometry)-1; wi++ {
		w1 := wayGeometry[wi]
		w2 := wayGeometry[wi+1]
		for ri := 0; ri < len(routePoints)-1; ri++ {
			r1 := LatLon{Lat: routePoints[ri].Latitude, Lon: routePoints[ri].Longitude}
			r2 := LatLon{Lat: routePoints[ri+1].Latitude, Lon: routePoints[ri+1].Longitude}

			if pt, ok := segmentIntersection(w1, w2, r1, r2); ok {
				crossings = append(crossings, pt)
			}

			pt, distSq := closestPointOnSegment(w1, w2, r1)
			if distSq < bestDistSq {
				bestDistSq = distSq
				closest = pt
			}
		}
	}
	return crossings, closest
}

func mainErr(file string, namePrefix string, split uint, workers int, retries int, failFast bool, cacheDir string, cacheTTL time.Duration, out string) error {
	if split == 0 {
		return fmt.Errorf("--split must be greater than 0")
	}

	ctx := context.Background()

	// Create and start the rate-limited Overpass client
	client := overpass.NewClient(
		"https://overpass-api.de/api/interpreter",
		"https://overpass-api.de/api/status",
	)
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("starting overpass client: %w", err)
	}
	defer client.Close()

	if workers == 0 {
		workers = client.RateLimit()
		log.Printf("Auto-detected %d workers from API rate limit", workers)
	}

	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("opening gpx file: %w", err)
	}
	gpx, err := gpxgo.Parse(f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("parsing gpx file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing gpx file: %w", err)
	}

	if len(gpx.Tracks) != 1 {
		return fmt.Errorf("expected gpx file to contain exactly one track but found %d", len(gpx.Tracks))
	}
	if len(gpx.Tracks[0].Segments) != 1 {
		return fmt.Errorf("expected gpx track to contain exactly one segment but found %d", len(gpx.Tracks[0].Segments))
	}

	stat, err := os.Stat(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(cacheDir, 0755); err != nil {
				return fmt.Errorf("creating cache dir at %s: %w", cacheDir, err)
			}
			log.Printf("created cache dir at %s", cacheDir)
		} else {
			return fmt.Errorf("checking cache dir at %s: %w", cacheDir, err)
		}
	} else if !stat.IsDir() {
		return fmt.Errorf("cache dir at %s is not a directory", cacheDir)
	}

	pts := gpx.Tracks[0].Segments[0].Points

	// TODO(glynternet): can use glynternet gpx package here instead
	chunkSize := len(pts) / int(split)
	if chunkSize < 1 {
		chunkSize = 1
	}
	var splits [][]gpxgo.GPXPoint
	for i := 0; i < len(pts); i += chunkSize {
		end := i + chunkSize
		if end > len(pts) {
			end = len(pts)
		}
		splits = append(splits, pts[i:end])
	}

	log.Println("points:", len(pts))

	var workUnits []workUnit
	for splitI, splitPoints := range splits {
		workUnits = append(workUnits, workUnit{
			splitIndex:  splitI,
			queries:     queries,
			routePoints: splitPoints,
		})
	}

	log.Printf("Processing %d splits with %d workers", len(workUnits), workers)

	retryConf := retryConfig{
		maxRetries: retries,
		baseDelay:  5 * time.Second,
		maxDelay:   60 * time.Second,
	}
	processUnits := concurrentUnitsWorker(workers, unitProcessor(ctx, cacheDir, cacheTTL, client.Query, retrier[[]element](retryConf)), failFast)
	results, err := processUnits(workUnits...)
	if err != nil {
		return err
	}

	slices.SortFunc(results, func(a, b workResult) int {
		return cmp.Compare(a.splitIndex, b.splitIndex)
	})

	// Collect POIs (sequential - no mutex needed)
	getPoint, getStats := point(namePrefix)

	pois := make(map[string]Point)
	addPoint := func(id int64, tags map[string]interface{}, loc LatLon) error {
		pt, err := getPoint(id, tags, loc)
		if err != nil {
			return fmt.Errorf("getting point for item: %w", err)
		}
		// TODO: better hash function where field order is guaranteed,
		//   i.e. json spec does not guarantee field order
		hash, err := json.Marshal(pt)
		if err != nil {
			return fmt.Errorf("marshalling point for node hash(%v): %w", pt, err)
		}
		pois[string(hash)] = pt
		return nil
	}
	for _, result := range results {
		for _, node := range result.nodes {
			if err := addPoint(node.ID, node.Tags, LatLon{Lat: node.Lat, Lon: node.Lon}); err != nil {
				return fmt.Errorf("adding point for node(%v): %w", node, err)
			}
		}
		for _, wp := range result.wayPoints {
			if err := addPoint(wp.ID, wp.Tags, wp.Loc); err != nil {
				return fmt.Errorf("adding point for wayPoint(%v): %w", wp, err)
			}
		}
	}

	var w io.Writer
	var wClose func() error
	switch out {
	case "":
		f, err := os.CreateTemp("", "pois-json")
		if err != nil {
			return fmt.Errorf("creating temp file for output: %w", err)
		}
		w = f
		wClose = f.Close
	case "-":
		w = os.Stdout
	default:
		f, err := os.OpenFile(out, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("opening file (%s) for writing: %w", out, err)
		}
		w = f
		wClose = f.Close
	}

	if err := writePois(pois, getStats, w); err != nil {
		if wClose != nil {
			_ = wClose()
		}
		return fmt.Errorf("writing pois: %w", err)
	}
	if wClose != nil {
		if err := wClose(); err != nil {
			return fmt.Errorf("closing output json writer: %w", err)
		}
	}

	stats := getStats(20)
	for _, occurrence := range stats.tagOccurrences {
		log.Println("Tag:", occurrence.value, "=>", occurrence.freq)
	}
	for _, occurrence := range stats.tagValueOccurrences {
		log.Println("Tag-Value:", occurrence.value, "=>", occurrence.freq)
	}

	return nil
}

func writePois(pois map[string]Point, getStats func(topK int) stats, out io.Writer) error {
	sortedPOIs :=
		slices.SortedFunc(maps.Values(pois), func(i, j Point) int {
			if i.Name != j.Name {
				return cmp.Compare(i.Name, j.Name)
			}
			if i.Description != j.Description {
				return cmp.Compare(i.Description, j.Description)
			}
			if i.Symbol != j.Symbol {
				return cmp.Compare(i.Symbol, j.Symbol)
			}
			if i.Lat != j.Lat {
				return cmp.Compare(i.Lat, j.Lat)
			}
			if i.Lon != j.Lon {
				return cmp.Compare(i.Lon, j.Lon)
			}
			if comparison := slices.Compare(i.Categories, j.Categories); comparison != 0 {
				return comparison
			}
			return cmp.Compare(i.OSMID, j.OSMID)

		})
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(sortedPOIs); err != nil {
		return fmt.Errorf("writing output json: %w", err)
	}

	if stats := false; stats {
		const topK = 50
		stats := getStats(topK)
		log.Println("top", topK, "tags")
		for _, tagOccurrence := range stats.tagOccurrences {
			log.Println("-", tagOccurrence.freq, tagOccurrence.value)
		}
		log.Println("top", topK, "tag values")
		for _, tagValueOccurrence := range stats.tagValueOccurrences {
			log.Println("-", tagValueOccurrence.freq, tagValueOccurrence.value)
		}
	}

	log.Println("pois:", len(pois))
	return nil
}

type stats struct {
	totalPoints         int
	tagOccurrences      []valueFreq[string]
	tagValueOccurrences []valueFreq[string]
}

type valueFreq[T comparable] struct {
	value T
	freq  int
}

type occurrences[T comparable] map[T]int

func (os occurrences[T]) mark(v T) {
	os[v]++
}

func (os occurrences[T]) markN(v T, n int) {
	os[v] += n
}

func (os occurrences[T]) topK(k int) []valueFreq[T] {
	var vfs []valueFreq[T]
	for tag, freq := range os {
		vfs = append(vfs, valueFreq[T]{
			value: tag,
			freq:  freq,
		})
	}
	sort.Slice(vfs, func(i, j int) bool {
		// descending
		return vfs[i].freq > vfs[j].freq
	})
	if len(vfs) > k {
		vfs = vfs[:k]
	}
	return vfs
}

func point(namePrefix string) (func(id int64, tags map[string]interface{}, latLon LatLon) (Point, error), func(topK int) stats) {
	var totalPoints int
	tagOccurrences := make(occurrences[string])
	tagValueOccurrences := make(occurrences[string])
	return func(id int64, tags map[string]interface{}, latLon LatLon) (Point, error) {
			name, err := resolveName(tags)
			if err != nil {
				return Point{}, fmt.Errorf("resolving name from tags(%v): %w", tags, err)
			}
			desc, err := json.Marshal(tags)
			if err != nil {
				return Point{}, fmt.Errorf("marshalling node tags for description: %w", err)
			}
			for tag, value := range tags {
				tagOccurrences.mark(tag)
				tagValueOccurrences.mark(tag + ":" + value.(string))
			}
			symbol, cats := resolveSymbolAndCategories(tags)
			if symbol == "" {
				log.Println("No symbol found for tags", tags)
			}
			nodePoint := Point{
				OSMID:       id,
				Name:        namePrefix + name,
				Lat:         latLon.Lat,
				Lon:         latLon.Lon,
				Description: string(desc),
				Symbol:      symbol,
				Categories:  cats,
			}
			totalPoints++
			return nodePoint, nil
		}, func(topK int) stats {
			return stats{
				totalPoints:         totalPoints,
				tagOccurrences:      tagOccurrences.topK(topK),
				tagValueOccurrences: tagValueOccurrences.topK(topK),
			}
		}
}

func queryRouteFilter(locus int, route []gpxgo.GPXPoint) (string, error) {
	if len(route) == 0 {
		return "", fmt.Errorf(`no route points provided`)
	}
	var sb strings.Builder
	sb.WriteString(`(around:` + strconv.Itoa(locus) + `,`)
	sb.WriteString(strconv.FormatFloat(route[0].Latitude, 'f', 6, 64))
	sb.WriteString(`,`)
	sb.WriteString(strconv.FormatFloat(route[0].Longitude, 'f', 6, 64))
	for _, p := range route[1:] {
		sb.WriteString(`,`)
		sb.WriteString(strconv.FormatFloat(p.Latitude, 'f', 6, 64))
		sb.WriteString(`,`)
		sb.WriteString(strconv.FormatFloat(p.Longitude, 'f', 6, 64))
	}
	return sb.String(), nil
}

// renderUnionQuery builds a single Overpass QL union query that combines all
// query categories for both node and way element types. Each category gets its
// own radius-specific route filter. Using `out geom qt;` returns way geometry
// inline, avoiding the need for separate recurse queries.
func renderUnionQuery(queries []query, routePoints []gpxgo.GPXPoint) (string, error) {
	var sb strings.Builder
	// timeout:120 tells the Overpass server to abort after 120s, matching our
	// HTTP client timeout. Without this, the server default is 180s, meaning a
	// query can keep running (and occupying a rate-limit slot) after the client
	// has timed out and potentially retried with a new request.
	sb.WriteString("[out:json][timeout:120];\n(\n")

	for _, q := range queries {
		filters, err := renderConditionFilters(q.conditions)
		if err != nil {
			return "", fmt.Errorf("rendering condition filters for %+v: %w", q.conditions, err)
		}

		locus := 80
		if q.radius != 0 {
			locus = q.radius
		}

		routeFilter, err := queryRouteFilter(locus, routePoints)
		if err != nil {
			return "", fmt.Errorf("creating route filter: %w", err)
		}

		sb.WriteString("  node" + filters + routeFilter + ");\n")
		sb.WriteString("  way" + filters + routeFilter + ");\n")
	}

	sb.WriteString(");\nout geom qt;")
	return sb.String(), nil
}

// processWayElement converts a single way element into wayPoints by finding
// route crossings or the closest approach point.
func processWayElement(e element, routePoints []gpxgo.GPXPoint) []wayPoint {
	if e.Type != `way` {
		return nil
	}
	if len(e.Geometry) < 2 {
		// Degenerate way: use the single geometry point if available
		if len(e.Geometry) == 1 {
			return []wayPoint{{ID: e.ID, Loc: e.Geometry[0], Tags: e.Tags}}
		}
		return nil
	}

	crossings, closest := wayRouteIntersections(e.Geometry, routePoints)
	if len(crossings) > 0 {
		var result []wayPoint
		for _, c := range crossings {
			result = append(result, wayPoint{ID: e.ID, Loc: c, Tags: e.Tags})
		}
		return result
	}
	return []wayPoint{{ID: e.ID, Loc: closest, Tags: e.Tags}}
}

// renderConditionFilters renders conditions into Overpass QL bracket-filter
// syntax, e.g. `[amenity~"^(bar|cafe)$"]`.
func renderConditionFilters(conditions []condition) (string, error) {
	var sb strings.Builder
	for _, element := range conditions {
		var definedConditions int
		for _, condition := range []bool{
			len(element.notValues) > 0,
			len(element.values) > 0,
			element.exists != ExistsUndefined,
		} {
			if condition {
				definedConditions++
			}
		}
		if definedConditions > 1 {
			return "", fmt.Errorf("query element must contain only one condition: 'not', 'values' or 'exists': %+v", element)
		}
		var elementConditions []string
		switch {
		case len(element.values) > 0:
			elementConditions = []string{fmt.Sprintf(`%s~"^(%s)$"`, element.tag, strings.Join(element.values, "|"))}
		case len(element.notValues) > 0:
			for _, notValue := range element.notValues {
				elementConditions = append(elementConditions, fmt.Sprintf(`%s!="%s"`, element.tag, notValue))
			}
		case element.exists != ExistsUndefined:
			switch element.exists {
			case ExistsYes:
				elementConditions = []string{fmt.Sprintf(`%s`, element.tag)}
			case ExistsNo:
				elementConditions = []string{fmt.Sprintf(`!%s`, element.tag)}
			default:
				return "", fmt.Errorf("unsupported exists value: %+v", element.exists)
			}
		default:
			return "", fmt.Errorf("query element contains no conditions: %+v", element)
		}
		for _, elementCondition := range elementConditions {
			if _, err := sb.WriteString(`[` + elementCondition + `]`); err != nil {
				return "", fmt.Errorf("writing query element: %w", err)
			}
		}
	}
	return sb.String(), nil
}

// queryResponseElementsRaw takes a pre-rendered Overpass query string and handles
// caching, API execution, and JSON parsing of the response.
func queryResponseElementsRaw(
	ctx context.Context,
	cacheDir string,
	cacheTTL time.Duration,
	makeQueryRequest func(ctx context.Context, query string) (*http.Response, error),
	renderedQuery string,
) ([]element, error) {
	hasher := sha1.New()
	if _, err := hasher.Write([]byte(renderedQuery)); err != nil {
		return nil, fmt.Errorf("hashing query: %w", err)
	}
	sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))

	var rc io.ReadCloser
	queryStateFilePath := filepath.Join(cacheDir, sha)
	if info, err := os.Stat(queryStateFilePath); err == nil {
		if time.Since(info.ModTime()) > cacheTTL {
			log.Printf("cache expired (age %s > ttl %s): %s",
				time.Since(info.ModTime()).Round(time.Second), cacheTTL, renderedQuery[:min(80, len(renderedQuery))])
		} else {
			stored, err := os.Open(queryStateFilePath)
			if err != nil {
				return nil, fmt.Errorf("opening cached query state file(%s): %w", queryStateFilePath, err)
			}
			if debug {
				log.Printf("query fetched from cached result: %s", queryStateFilePath)
			}
			rc = stored
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("checking cache file(%s): %w", queryStateFilePath, err)
	}

	if rc == nil {
		log.Printf("query result not cached, making query to API: %s", renderedQuery[:min(80, len(renderedQuery))])
		resp, err := makeQueryRequest(ctx, renderedQuery)
		if err != nil {
			return nil, fmt.Errorf("posting query: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, &httpStatusError{statusCode: resp.StatusCode, status: resp.Status}
		}
		elements, err := atomicSlurp(cacheDir, resp.Body, queryStateFilePath)
		if err != nil {
			_ = resp.Body.Close()
			return elements, fmt.Errorf("storing content into cache: %w", err)
		}
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("closing response body: %w", err)
		}

		if debug {
			log.Printf("query result written: %s", queryStateFilePath)
		}
		stored, err := os.Open(queryStateFilePath)
		if err != nil {
			return nil, fmt.Errorf("opening cached result after write: %w", err)
		}
		rc = stored
	}

	var r response
	if err := json.NewDecoder(rc).Decode(&r); err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("decoding response body: %w", err)
	}
	if err := rc.Close(); err != nil {
		return nil, fmt.Errorf("closing response body: %w", err)
	}
	return r.Elements, nil
}

func atomicSlurp(cacheDir string, resp io.Reader, path string) ([]element, error) {
	tmpFile, err := os.CreateTemp(cacheDir, ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file for cache write: %w", err)
	}
	if _, err := io.Copy(tmpFile, resp); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("writing response body to temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpFile.Name(), path); err != nil {
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("renaming temp file to cache path: %w", err)
	}
	return nil, nil
}

func resolveName(tags map[string]interface{}) (string, error) {
	for _, tag := range []string{
		"name",
		"name:en",
		"int_name",
		"official_name",
		"alt_name",
		"amenity",
		"tourism",
		"leisure",
		"shop",
		"waterway",
		"natural",
		"boundary", // probably good to combine this with another tag
		"man_made",
		"drinking_water",
	} {
		n, ok := tags[tag]
		if ok {
			n, ok := n.(string)
			if !ok {
				return "", fmt.Errorf("tag '%s' is not a string", tag)
			}
			return n, nil
		}
	}
	var yesTags []string
	for tag, v := range tags {
		v, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("tag '%s' is not a string", tag)
		}
		if v == "yes" {
			yesTags = append(yesTags, tag)
		}
	}
	if len(yesTags) > 0 {
		slices.Sort(yesTags)
		log.Printf(`Resolved name from tags with value "yes": %s`, yesTags)
		return strings.Join(yesTags, " "), nil
	}
	return "", errors.New("no suitable tag for name")
}

func resolveSymbolAndCategories(tags map[string]interface{}) (string, []string) {
	// symbol is meant to be a symbol that make sense on a Garmin GPC
	// category is meant to be a more fine-grained category for an item
	var symbols []string
	var categories []string
	type matchConfig struct {
		exact  string
		any    []string
		exists bool
	}

	matchedTags := make(map[string]string)

	static := func(v string) func(_ map[string]string) string {
		return func(_ map[string]string) string {
			return v
		}
	}

	for _, symbolMatchers := range []struct {
		tags     map[string]matchConfig
		symbol   string
		category func(tags map[string]string) string // defaults to symbol if no category given
	}{
		{tags: map[string]matchConfig{"shop": {any: []string{"convenience", "supermarket"}}}, symbol: "Shopping Center", category: static("Resupply")},
		{tags: map[string]matchConfig{"leisure": {exact: "park"}}, symbol: "Park"},
		{tags: map[string]matchConfig{"boundary": {exact: "protected_area"}}, symbol: "Park"},
		{tags: map[string]matchConfig{"amenity": {exact: "toilets"}}, symbol: "Restroom", category: static("Toilets")},
		{tags: map[string]matchConfig{"amenity": {exact: "drinking_water"}}, symbol: "Drinking Water"},
		{tags: map[string]matchConfig{"natural": {any: []string{"peak", "saddle"}}}, symbol: "Summit"},
		{tags: map[string]matchConfig{"mountain_pass": {exact: "yes"}}, symbol: "Summit"},
		{tags: map[string]matchConfig{"tourism": {exact: "viewpoint"}}, symbol: "Scenic Area", category: static("Viewpoint")},
		{tags: map[string]matchConfig{"amenity": {exact: "bicycle_repair_station"}}, symbol: "Mine", category: static("Bicycle Repair Station")},
		{tags: map[string]matchConfig{"amenity": {exact: "fast_food"}}, symbol: "Fast Food", category: static("Restaurant")},
		{tags: map[string]matchConfig{"amenity": {exact: "fuel"}}, symbol: "Gas Station"},
		{tags: map[string]matchConfig{"amenity": {any: []string{"pub", "bar"}}}, symbol: "Bar", category: static("Restaurant")},
		{tags: map[string]matchConfig{"amenity": {exact: "cafe"}}, symbol: "Restaurant"},
		{tags: map[string]matchConfig{"shop": {exact: "coffee"}}, symbol: "Restaurant"},
		{tags: map[string]matchConfig{"tourism": {exact: "picnic_site"}}, symbol: "Picnic Area", category: static("Park")},
		{tags: map[string]matchConfig{"amenity": {exact: "restaurant"}, "cuisine": {exact: "pizza"}}, symbol: "Pizza", category: static("Restaurant")},
		{tags: map[string]matchConfig{"amenity": {exact: "restaurant"}}, symbol: "Restaurant"},
		{tags: map[string]matchConfig{"amenity": {exact: "ice_cream"}}, symbol: "Fast Food", category: static("Restaurant")},
		{tags: map[string]matchConfig{"tourism": {any: []string{"camp_pitch", "camp_site"}}}, symbol: "Campground"},
		{tags: map[string]matchConfig{"tourism": {any: []string{
			"alpine_hut",
			"guest_house",
			"hotel",
			"hostel",
			"motel",
			"wilderness_hut",
		}}}, symbol: "Building", category: static("Accommodation")},
		{tags: map[string]matchConfig{"accommodation": {exists: true}}, symbol: "Building", category: static("Accommodation")},
		{tags: map[string]matchConfig{"leisure": {exact: "nature_reserve"}}, symbol: "Park"},
		{tags: map[string]matchConfig{"amenity": {exact: "shelter"}}, symbol: "Building", category: static("Shelter")},
		{tags: map[string]matchConfig{"amenity": {exact: "place_of_worship"}}, symbol: "Church", category: static("Place of Worship")},
		{tags: map[string]matchConfig{"place": {any: []string{"town", "village", "hamlet", "city", "neighbourhood"}}}, symbol: "City Hall", category: static("Settlement")},
		{tags: map[string]matchConfig{"waterway": {any: []string{"river", "stream", "waterfall", "spring"}}}, symbol: "Water Source"},
		{tags: map[string]matchConfig{"ford": {exact: "yes"}}, symbol: "Water Source"},
		{tags: map[string]matchConfig{"amenity": {exists: true}}, category: func(tags map[string]string) string {
			v := tags["amenity"]
			if len(v) == 0 {
				return v
			}
			v = strings.ReplaceAll(v, "_", " ")
			v = cases.Title(language.Und).String(v)
			v = strings.ReplaceAll(v, " Of ", " of ") // things like Place Of Worship look weird with capital O
			return v
		}},
	} {
		match := true
		for k, matcher := range symbolMatchers.tags {
			// Tags are map[string]interface{} from JSON decoding. Use a type
			// assertion to string rather than comparing interface{} values
			// directly, which would silently fail if the JSON decoder ever
			// produced a non-string type for a tag value.
			v, ok := tags[k].(string)
			if !ok {
				match = false
				break
			}
			if !matcher.exists &&
				((matcher.exact != "" && matcher.exact != v) ||
					(len(matcher.any) > 0 && !slices.Contains(matcher.any, v))) {
				match = false
				break
			}
			matchedTags[k] = v
		}
		if match {
			if symbolMatchers.symbol != "" {
				symbols = append(symbols, symbolMatchers.symbol)
			}
			if symbolMatchers.category != nil {
				if category := symbolMatchers.category(matchedTags); category != "" {
					categories = append(categories, category)
				}
			} else if symbolMatchers.symbol != "" {
				categories = append(categories, symbolMatchers.symbol)
			}
		}
	}

	// do not sort symbols because they are in priority order
	// sort and compact to rid of duplicates

	symbols = slices.CompactFunc(symbols, orderRetainingUniqCompact[string]())
	// do sort categories because they are a set
	slices.Sort(categories)
	categories = slices.Compact(categories)
	if len(symbols) == 0 {
		return "", categories
	}
	if len(symbols) > 1 {
		log.Printf("Multiple symbols matched for tags (%v), using first: %v", matchedTags, symbols)
	}
	return symbols[0], categories
}

// orderRetainingUniqCompact provides a function to pass to slices.CompactFunc that will compact a slice in a way that
// retains only the first instance of a seen value.
// e.g. ["a", "b", "a", "c", "c", "a", "b"] will compact to ["a", "b", "c"]
func orderRetainingUniqCompact[E comparable]() func(E, E) bool {
	seen := make(map[E]struct{})
	return func(next E, prev E) bool {
		if next == prev {
			seen[next] = struct{}{}
			return true
		}
		_, ok := seen[next]
		if !ok {
			seen[next] = struct{}{}
		}
		return ok
	}
}
