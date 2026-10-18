package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	// aliased because runRoute binds the parsed file to a local named gpx
	glyngpx "github.com/glynternet/gpx/pkg/gpx"
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
	Type     string            `json:"type"`
	ID       int64             `json:"id"`
	Lat      float64           `json:"lat"`
	Lon      float64           `json:"lon"`
	Nodes    []int64           `json:"nodes"`
	Tags     map[string]string `json:"tags"`
	Geometry []LatLon          `json:"geometry"`
	Members  []member          `json:"members"`
}

type member struct {
	Type     string   `json:"type"`
	Ref      int64    `json:"ref"`
	Role     string   `json:"role"`
	Lat      float64  `json:"lat"`
	Lon      float64  `json:"lon"`
	Geometry []LatLon `json:"geometry"`
}

// MODEL

type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type wayPoint struct {
	Type string // "way" or "relation"
	ID   int64
	Loc  LatLon
	Tags map[string]string
}

// Point is the internal representation of a resolved POI, collected before being
// written out as a GeoJSON Feature (see writePois). It is not the output schema.
type Point struct {
	OSMID   int64
	OSMType string // "node", "way" or "relation"
	Name    string
	Lat     float64
	Lon     float64
	// Category is the primary category in matcher priority order; the map uses it
	// to choose an icon. Categories is the full sorted set, used for filtering.
	Category   string
	Categories []string
	// Tags carries the raw OSM tags as structured key/value pairs so downstream
	// consumers (e.g. the triage UI) can filter on them directly.
	Tags map[string]string
}

// GeoJSON output types (RFC 7946). The CRS is implicitly WGS84 (lon/lat degrees),
// so no `crs` member is emitted. coordinates are [longitude, latitude].
type featureCollection struct {
	Type     string    `json:"type"` // always "FeatureCollection"
	Features []feature `json:"features"`
}

type feature struct {
	Type string `json:"type"` // always "Feature"
	// ID namespaces the OSM element type with its id (e.g. "node/123"), giving a
	// stable, collision-free Feature id (node/way/relation ids share a space).
	ID         string            `json:"id"`
	Geometry   geometry          `json:"geometry"`
	Properties featureProperties `json:"properties"`
}

type geometry struct {
	Type        string     `json:"type"`        // always "Point"
	Coordinates [2]float64 `json:"coordinates"` // [longitude, latitude]
}

type featureProperties struct {
	Name string `json:"name"`
	// Category is the primary (priority-ordered) category, used to pick the icon.
	Category   string            `json:"category"`
	Categories []string          `json:"categories"`
	OSMType    string            `json:"osm_type"`
	OSMID      int64             `json:"osmid"`
	Tags       map[string]string `json:"tags"`
}

func geoJSONFeature(p Point) feature {
	return feature{
		Type: "Feature",
		ID:   fmt.Sprintf("%s/%d", p.OSMType, p.OSMID),
		Geometry: geometry{
			Type:        "Point",
			Coordinates: [2]float64{round6(p.Lon), round6(p.Lat)},
		},
		Properties: featureProperties{
			Name:       p.Name,
			Category:   p.Category,
			Categories: p.Categories,
			OSMType:    p.OSMType,
			OSMID:      p.OSMID,
			Tags:       p.Tags,
		},
	}
}

// round6 rounds a coordinate to 6 decimal places (~0.1 m), the precision
// recommended by RFC 7946 §11.2; more digits only inflate the output.
func round6(f float64) float64 {
	return math.Round(f*1e6) / 1e6
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

// endpointSpec describes one Overpass server configured via --overpass-endpoint.
// Concurrency is only consulted when the server reports Rate limit: 0 (unlimited).
type endpointSpec struct {
	Name        string
	Interpreter string
	Status      string
	Concurrency int // used only for unlimited servers; 0 means use default
}

// endpointFlag implements pflag.Value for repeatable --overpass-endpoint.
// Each value is NAME=INTERPRETER_URL,STATUS_URL[,CONCURRENCY].
type endpointFlag struct {
	specs []endpointSpec
	set   bool
}

func (f *endpointFlag) String() string {
	var parts []string
	for _, s := range f.specs {
		parts = append(parts, s.Name)
	}
	return strings.Join(parts, ",")
}

func (f *endpointFlag) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("expected NAME=INTERPRETER,STATUS[,CONCURRENCY], got %q", v)
	}
	parts := strings.Split(rest, ",")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("expected 2 or 3 comma-separated values after %q=, got %d", name, len(parts))
	}
	spec := endpointSpec{Name: name, Interpreter: parts[0], Status: parts[1]}
	if len(parts) == 3 {
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 1 {
			return fmt.Errorf("invalid concurrency %q for endpoint %q: must be positive integer", parts[2], name)
		}
		spec.Concurrency = n
	}
	f.specs = append(f.specs, spec)
	f.set = true
	return nil
}

const defaultUnlimitedConcurrency = 4

func defaultEndpoints() []endpointSpec {
	return []endpointSpec{
		{
			Name:        "de",
			Interpreter: "https://overpass-api.de/api/interpreter",
			Status:      "https://overpass-api.de/api/status",
		},
		{
			Name:        "pc",
			Interpreter: "https://overpass.private.coffee/api/interpreter",
			Status:      "https://overpass.private.coffee/api/status",
			Concurrency: defaultUnlimitedConcurrency,
		},
	}
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

func runRoute(ctx context.Context, stdout io.Writer, file string, namePrefix string, split uint, workers int, retries int, failFast bool, cacheDir string, cacheTTL time.Duration, out string, endpoints []endpointSpec) error {
	if split == 0 {
		return fmt.Errorf("--split must be greater than 0")
	}

	const queryTimeout = 180 * time.Second

	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("opening gpx file: %w", err)
	}
	gpx, err := gpxgo.Parse(f)
	if err != nil {
		_ = f.Close() // Preserve the parsing error if closing also fails.
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
	if len(pts) == 0 {
		return fmt.Errorf("gpx track segment contains no points")
	}

	log.Println("points:", len(pts))

	// No pre-overlap: neighbouring chunks share only their boundary point.
	splits, err := glyngpx.SplitPoints(pts, split, 0)
	if err != nil {
		return fmt.Errorf("splitting route into chunks: %w", err)
	}

	var workUnits []workUnit
	for splitI, splitPoints := range splits {
		workUnits = append(workUnits, workUnit{
			splitIndex:  splitI,
			queries:     queries,
			routePoints: splitPoints,
		})
	}

	log.Printf("Processing %d splits", len(workUnits))

	orchestrator := queryOrchestrator{
		cache:        fileQueryCache{dir: cacheDir, ttl: cacheTTL},
		queryTimeout: queryTimeout,
		retry: retryConfig{
			maxRetries: retries,
			baseDelay:  5 * time.Second,
			maxDelay:   60 * time.Second,
		},
		failFast: failFast,
		workers:  workers,
		startClients: func(ctx context.Context) (<-chan clientWorkers, func()) {
			return provisionClients(ctx, endpoints, queryTimeout)
		},
	}
	results, err := orchestrator.Run(ctx, workUnits)
	if err != nil {
		return err
	}

	slices.SortFunc(results, func(a, b workResult) int {
		return cmp.Compare(a.splitIndex, b.splitIndex)
	})

	// Collect POIs (sequential - no mutex needed)
	getPoint, getStats := point(namePrefix)

	pois := make(map[string]Point)
	addPoint := func(osmType string, id int64, tags map[string]string, loc LatLon) error {
		pt, err := getPoint(osmType, id, tags, loc)
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
			if err := addPoint("node", node.ID, node.Tags, LatLon{Lat: node.Lat, Lon: node.Lon}); err != nil {
				return fmt.Errorf("adding point for node(%v): %w", node, err)
			}
		}
		for _, wp := range result.wayPoints {
			if err := addPoint(wp.Type, wp.ID, wp.Tags, wp.Loc); err != nil {
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
		w = stdout
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
			_ = wClose() // Preserve the output error if closing also fails.
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
			if i.Category != j.Category {
				return cmp.Compare(i.Category, j.Category)
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
			if i.OSMType != j.OSMType {
				return cmp.Compare(i.OSMType, j.OSMType)
			}
			return cmp.Compare(i.OSMID, j.OSMID)

		})

	fc := featureCollection{Type: "FeatureCollection", Features: make([]feature, 0, len(sortedPOIs))}
	for _, p := range sortedPOIs {
		fc.Features = append(fc.Features, geoJSONFeature(p))
	}

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(fc); err != nil {
		return fmt.Errorf("writing output geojson: %w", err)
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

func point(namePrefix string) (func(osmType string, id int64, tags map[string]string, latLon LatLon) (Point, error), func(topK int) stats) {
	var totalPoints int
	tagOccurrences := make(occurrences[string])
	tagValueOccurrences := make(occurrences[string])
	return func(osmType string, id int64, tags map[string]string, latLon LatLon) (Point, error) {
			name, err := resolveName(tags)
			if err != nil {
				return Point{}, fmt.Errorf("resolving name from tags(%v): %w", tags, err)
			}
			for tag, value := range tags {
				tagOccurrences.mark(tag)
				tagValueOccurrences.mark(tag + ":" + value)
			}
			category, cats := resolveCategories(tags)
			if category == "" {
				log.Println("No category found for tags", tags)
			}
			nodePoint := Point{
				OSMID:      id,
				OSMType:    osmType,
				Name:       namePrefix + name,
				Lat:        latLon.Lat,
				Lon:        latLon.Lon,
				Category:   category,
				Categories: cats,
				Tags:       tags,
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
func renderUnionQuery(queries []query, routePoints []gpxgo.GPXPoint, timeout time.Duration) (string, error) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[out:json][timeout:%d];\n(\n", int(timeout.Seconds())))

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
		sb.WriteString("  rel" + filters + routeFilter + ");\n")
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
			return []wayPoint{{Type: e.Type, ID: e.ID, Loc: e.Geometry[0], Tags: e.Tags}}
		}
		return nil
	}

	crossings, closest := wayRouteIntersections(e.Geometry, routePoints)
	if len(crossings) > 0 {
		var result []wayPoint
		for _, c := range crossings {
			result = append(result, wayPoint{Type: e.Type, ID: e.ID, Loc: c, Tags: e.Tags})
		}
		return result
	}
	return []wayPoint{{Type: e.Type, ID: e.ID, Loc: closest, Tags: e.Tags}}
}

// processRelationElement converts a relation element into wayPoints by
// processing each way member's geometry independently for route crossings.
func processRelationElement(e element, routePoints []gpxgo.GPXPoint) []wayPoint {
	var allCrossings []LatLon
	var globalClosest *LatLon
	globalClosestDistSq := math.Inf(1)
	hadWayGeometry := false

	for _, m := range e.Members {
		if m.Type != "way" || len(m.Geometry) < 2 {
			continue
		}
		hadWayGeometry = true
		crossings, closest := wayRouteIntersections(m.Geometry, routePoints)
		allCrossings = append(allCrossings, crossings...)

		// Find minimum squared distance from this member's closest point to any route point
		minDistSq := math.Inf(1)
		for _, rp := range routePoints {
			dLat := closest.Lat - rp.Latitude
			dLon := closest.Lon - rp.Longitude
			distSq := dLat*dLat + dLon*dLon
			if distSq < minDistSq {
				minDistSq = distSq
			}
		}
		if minDistSq < globalClosestDistSq {
			c := closest
			globalClosest = &c
			globalClosestDistSq = minDistSq
		}
	}

	if hadWayGeometry {
		if len(allCrossings) > 0 {
			var result []wayPoint
			for _, c := range allCrossings {
				result = append(result, wayPoint{Type: e.Type, ID: e.ID, Loc: c, Tags: e.Tags})
			}
			return result
		}
		if globalClosest != nil {
			return []wayPoint{{Type: e.Type, ID: e.ID, Loc: *globalClosest, Tags: e.Tags}}
		}
		return nil
	}

	// Fallback: use node members as point locations
	var result []wayPoint
	for _, m := range e.Members {
		if m.Type == "node" && (m.Lat != 0 || m.Lon != 0) {
			result = append(result, wayPoint{
				Type: e.Type,
				ID:   e.ID,
				Loc:  LatLon{Lat: m.Lat, Lon: m.Lon},
				Tags: e.Tags,
			})
		}
	}
	return result
}

// renderConditionFilters renders conditions into Overpass QL bracket-filter
// syntax, e.g. `[amenity~"^(bar|cafe)$"]`.
//
// Note: Overpass QL does not accept empty-string equality like `[key=""]`
// (see https://github.com/drolbr/Overpass-API/issues/92), so callers cannot
// match tags whose value is the empty string via `values`. The negated form
// `[key!=""]` is accepted and is the idiomatic way to require a non-empty
// value (use `notValues: []string{""}`).
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

func resolveName(tags map[string]string) (string, error) {
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
		"ford", // should actually check that it's not ford:yes
	} {
		if n, ok := tags[tag]; ok {
			if n == "yes" {
				continue
			}
			return n, nil
		}
	}
	var yesTags []string
	for tag, v := range tags {
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

// resolveCategories matches a POI's tags against an ordered list of rules and
// returns the primary category (the first match in priority order, used to pick
// the map icon) plus the full sorted set of matched categories (used for
// filtering). A POI can sit in several categories at once.
func resolveCategories(tags map[string]string) (primary string, all []string) {
	var ordered []string // matched categories in rule priority order
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

	for _, rule := range []struct {
		tags     map[string]matchConfig
		category func(tags map[string]string) string
	}{
		{tags: map[string]matchConfig{"shop": {any: []string{"convenience", "supermarket", "general", "greengrocer", "food", "health_food", "kiosk", "tortilla"}}}, category: static("Resupply")},
		{tags: map[string]matchConfig{"shop": {any: []string{"bakery", "pastry"}}}, category: static("Bakery")},
		{tags: map[string]matchConfig{"shop": {any: []string{"dairy", "cheese"}}}, category: static("Dairy")},
		{tags: map[string]matchConfig{"shop": {exact: "farm"}}, category: static("Farm Shop")},
		{tags: map[string]matchConfig{"shop": {exact: "ice_cream"}}, category: static("Ice Cream")},
		{tags: map[string]matchConfig{"shop": {exact: "chemist"}}, category: static("Pharmacy")},
		{tags: map[string]matchConfig{"shop": {exact: "bicycle"}}, category: static("Bicycle Shop")},
		{tags: map[string]matchConfig{"shop": {exact: "sports"}}, category: static("Sports Shop")},
		{tags: map[string]matchConfig{"shop": {exact: "water"}}, category: static("Drinking Water")},
		{tags: map[string]matchConfig{"leisure": {exact: "park"}}, category: static("Park")},
		{tags: map[string]matchConfig{"boundary": {exact: "protected_area"}}, category: static("Park")},
		{tags: map[string]matchConfig{"amenity": {exact: "toilets"}}, category: static("Toilets")},
		{tags: map[string]matchConfig{"amenity": {exact: "drinking_water"}}, category: static("Drinking Water")},
		{tags: map[string]matchConfig{"man_made": {any: []string{"water_tap", "drinking_fountain"}}}, category: static("Drinking Water")},
		{tags: map[string]matchConfig{"drinking_water": {exact: "yes"}}, category: static("Drinking Water")},
		{tags: map[string]matchConfig{"amenity": {any: []string{"water_point", "watering_place"}}}, category: static("Drinking Water")},
		{tags: map[string]matchConfig{"man_made": {any: []string{"water_well", "spring_box"}}}, category: static("Water Source")},
		{tags: map[string]matchConfig{"amenity": {exact: "fountain"}}, category: static("Drinking Water")},
		{tags: map[string]matchConfig{"natural": {any: []string{"peak", "saddle"}}}, category: static("Summit")},
		{tags: map[string]matchConfig{"mountain_pass": {exact: "yes"}}, category: static("Summit")},
		{tags: map[string]matchConfig{"natural": {exact: "mountain_range"}}, category: static("Mountain Range")},
		{tags: map[string]matchConfig{"tourism": {exact: "viewpoint"}}, category: static("Viewpoint")},
		{tags: map[string]matchConfig{"amenity": {exact: "bicycle_repair_station"}}, category: static("Bicycle Repair Station")},
		{tags: map[string]matchConfig{"amenity": {exact: "fast_food"}}, category: static("Restaurant")},
		{tags: map[string]matchConfig{"amenity": {exact: "fuel"}}, category: static("Gas Station")},
		{tags: map[string]matchConfig{"amenity": {any: []string{"pub", "bar"}}}, category: static("Restaurant")},
		{tags: map[string]matchConfig{"amenity": {exact: "cafe"}}, category: static("Restaurant")},
		{tags: map[string]matchConfig{"shop": {exact: "coffee"}}, category: static("Restaurant")},
		{tags: map[string]matchConfig{"tourism": {exact: "picnic_site"}}, category: static("Park")},
		{tags: map[string]matchConfig{"amenity": {exact: "restaurant"}, "cuisine": {exact: "pizza"}}, category: static("Restaurant")},
		{tags: map[string]matchConfig{"amenity": {exact: "restaurant"}}, category: static("Restaurant")},
		{tags: map[string]matchConfig{"amenity": {exact: "ice_cream"}}, category: static("Restaurant")},
		{tags: map[string]matchConfig{"tourism": {any: []string{"camp_pitch", "camp_site"}}}, category: static("Campground")},
		{tags: map[string]matchConfig{"tourism": {any: []string{
			"alpine_hut",
			"guest_house",
			"hotel",
			"hostel",
			"motel",
			"wilderness_hut",
		}}}, category: static("Accommodation")},
		{tags: map[string]matchConfig{"accommodation": {exists: true}}, category: static("Accommodation")},
		{tags: map[string]matchConfig{"leisure": {exact: "nature_reserve"}}, category: static("Park")},
		{tags: map[string]matchConfig{"amenity": {exact: "shelter"}}, category: static("Shelter")},
		{tags: map[string]matchConfig{"amenity": {exact: "place_of_worship"}}, category: static("Place of Worship")},
		{tags: map[string]matchConfig{"information": {exact: "visitor_centre"}}, category: static("Visitor Centre")},
		{tags: map[string]matchConfig{"information": {exact: "office"}}, category: static("Tourist Office")},
		{tags: map[string]matchConfig{"place": {any: []string{"town", "village", "hamlet", "city", "neighbourhood"}}}, category: static("Settlement")},
		{tags: map[string]matchConfig{"natural": {exact: "spring"}}, category: static("Water Source")},
		{tags: map[string]matchConfig{"waterway": {exact: "spring"}}, category: static("Water Source")},
		{tags: map[string]matchConfig{"ford": {exact: "yes"}}, category: static("Water Source")},
		// Flowing watercourses get their own category (rendered with a waves icon),
		// distinct from point "Water Source" features (springs/wells/fords).
		// `artificial`/`connector` are NHD flow-path types (US imports) that are
		// really named creeks/rivers. This rule sits late, so a more specific
		// category still wins when one matches.
		{tags: map[string]matchConfig{"waterway": {any: []string{"river", "stream", "waterfall", "artificial", "connector", "rapids", "tidal_channel"}}}, category: static("Waterway")},
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
		for k, matcher := range rule.tags {
			v, ok := tags[k]
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
			if category := rule.category(matchedTags); category != "" {
				ordered = append(ordered, category)
			}
		}
	}

	// ordered is in rule priority order; keep only the first occurrence of each so
	// the primary is the highest-priority match.
	ordered = slices.CompactFunc(ordered, orderRetainingUniqCompact[string]())
	if len(ordered) == 0 {
		return "", nil
	}
	// all is the sorted set used for filtering; primary keeps priority order.
	all = slices.Clone(ordered)
	slices.Sort(all)
	return ordered[0], all
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
