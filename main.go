package main

import (
	"cmp"
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
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	gpxgo "github.com/tkrajina/gpxgo/gpx"
)

const (
	dataDir = `/tmp/route-poi-finder-state`

	debug = false
)

const (
	// TODO(glynternet): workout better API
	ExistsUndefined = iota
	ExistsYes
	ExistsNo
)

type query struct {
	radius     int
	conditions []condition
}

type condition struct {
	tag       string
	values    []string
	notValues []string
	exists    int
}

var queries = []query{{
	radius: 1000,
	conditions: []condition{{
		tag: "amenity",
		values: []string{
			"bar",
			"biergarten",
			"cafe",
			"fast_food",
			"food_court",
			"fountain",
			"fuel",
			"ice_cream",
			"marketplace",
			"pub",
			"restaurant",
		},
	}},
}, {
	radius: 500,
	conditions: []condition{{
		tag: "amenity",
		values: []string{
			"bicycle_rental",
			"bicycle_repair_station",
			"bicycle_wash",
			"compressed_air",
			"place_of_worship",
			"public_bath",
			"shelter",
			"shower",
			"toilets",
		},
	}},
}, {
	radius: 200,
	conditions: []condition{{
		tag: "amenity",
		values: []string{
			"drinking_water",
			"water_point",
			"watering_place",
		},
	}},
}, {
	radius: 200,
	conditions: []condition{{
		// - - tourism~"^(alpine_hut|camp_pitch|camp_site|guest_house|hostel|picnic_site|viewpoint|wilderness_hut)$"
		tag: "tourism",
		values: []string{
			"alpine_hut",
			"camp_pitch",
			"camp_site",
			"guest_house",
			"hostel",
			"wilderness_hut",
		},
	}},
}, {
	conditions: []condition{{
		// - - tourism~"^(alpine_hut|camp_pitch|camp_site|guest_house|hostel|picnic_site|viewpoint|wilderness_hut)$"
		tag: "tourism",
		values: []string{
			"picnic_site",
			"viewpoint",
		},
	}},
}, {
	conditions: []condition{{
		// - - leisure~"^(nature_reserve|park|picnic_table|wildlife_hide)$"
		tag: "leisure",
		values: []string{
			"nature_reserve",
			"park",
			"picnic_table",
			"wildlife_hide",
		},
	}},
}, {
	conditions: []condition{{
		// - - natural~"^(spring|peak)$"
		tag: "natural",
		values: []string{
			"spring",
			"peak",
			"mountain_range",
			"ridge",
			"arete",
			"hot spring",
			"plateu",
			"saddle",
		},
	}},
}, {
	conditions: []condition{{
		//boundary=aboriginal_lands
		//boundary=national_park
		//boundary=forest
		//boundary=water_protection_area
		//boundary=protected_area
		tag: "boundary",
		values: []string{
			"protected_area",
			"aboriginal_lands",
			"national_park",
			"forest",
			"water_protection_area",
		},
	}},
}, {
	radius: 1000,
	conditions: []condition{{
		// - - man_made~"^(spring_box|water_well|water_tap)$"
		tag: "man_made",
		values: []string{
			"spring_box",
			"water_well",
			"water_tap",
			"drinking_fountain",
		},
	}},
}, {
	radius: 1000,
	conditions: []condition{{
		tag: "drinking_water",
		values: []string{
			"yes",
		},
	}},
}, {
	conditions: []condition{{
		tag:    "waterway",
		exists: ExistsYes,
	}, {
		tag: "waterway",
		notValues: []string{
			"drain",
			"dam",
			"stream", // may be good but is too high frequency to deal with atm
			"ditch",
			"canal",
		},
	}},
}, {
	conditions: []condition{{
		// - - place~"^(town|village|hamlet|city|neighbourhood)$"
		tag: "place",
		values: []string{
			"town",
			"village",
			"hamlet",
			"city",
			"neighbourhood",
		},
	}},
}, {
	radius: 200,
	//- - amenity="fountain"
	//  - drinking_water!="no"
	//  - drinking_water~".+"
	conditions: []condition{{
		tag:    "amenity",
		values: []string{"fountain"},
	}, {
		tag:    "drinking_water",
		exists: ExistsYes,
	}, {
		tag:       "drinking_water",
		notValues: []string{"no"},
	}},
}, {
	radius: 2000,
	conditions: []condition{{
		tag: "shop",
		values: []string{
			"bakery",
			"cheese",
			"coffee",
			"convenience",
			"dairy",
			"farm",
			"food",
			"greengrocer",
			"health_food",
			"ice_cream",
			"pastry",
			"tortilla",
			"water",
			"general",
			"kiosk",
			"supermarket",
			"chemist",
			"bicycle",
			"sports",
		},
	}},
}, {
	conditions: []condition{{
		tag:    "mountain_pass",
		values: []string{"yes"},
	}}},
}

// API

type response struct {
	Elements []element `json:"elements"`
}

type element struct {
	Type  string                 `json:"type"`
	ID    int64                  `json:"id"`
	Lat   float64                `json:"lat"`
	Lon   float64                `json:"lon"`
	Nodes []int64                `json:"nodes"`
	Tags  map[string]interface{} `json:"tags"`
}

// MODEL

type LatLon struct {
	Lat float64
	Lon float64
}

type wayCentre struct {
	ID     int64
	Centre LatLon
	Tags   map[string]interface{}
}

// Point is stolen from gpx project, should really import it instead
type Point struct {
	// field names matched to GPX spec
	Name        string  `json:"name"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Description string  `json:"desc"`
	Symbol      string  `json:"sym"`
}

func main() {
	log.SetFlags(log.Ldate | log.Lmicroseconds | log.Lshortfile)
	// flags have to go before args
	// TODO(glynternet): use better flags package
	namePrefix := flag.String(`name-prefix`, ``, `prefix to place in front of all points`)
	split := flag.Uint(`split`, 5, `number of segments to split track into for querying overpass API`)
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {
		log.Println("must provide gpx file arg")
		os.Exit(1)
	}
	if err := mainErr(args[0], *namePrefix, *split); err != nil {
		log.Println(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func mainErr(file string, namePrefix string, split uint) error {
	if split == 0 {
		return fmt.Errorf("--split must be greater than 0")
	}

	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("opening gpx file: %w", err)
	}

	gpx, err := gpxgo.Parse(f)
	if err != nil {
		return fmt.Errorf("parsing gpx file: %w", err)
	}

	if len(gpx.Tracks) != 1 {
		return fmt.Errorf("expected gpx file to contain exactly one track but found %d", len(gpx.Tracks))
	}
	if len(gpx.Tracks[0].Segments) != 1 {
		return fmt.Errorf("expected gpx track to contain exactly one segment but found %d", len(gpx.Tracks[0].Segments))
	}

	stat, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(dataDir, 0755); err != nil {
				return fmt.Errorf("creating data dir at %s: %w", dataDir, err)
			}
			log.Printf("created data dir at %s", dataDir)
		} else {
			return fmt.Errorf("checking data dir at %s: %w", dataDir, err)
		}
	} else if !stat.IsDir() {
		return fmt.Errorf("data dir at %s is not a directory", dataDir)
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

	getPoint, getStats := point(namePrefix)
	pois := make(map[Point]struct{})
	queryPointCounts := make(occurrences[string])

	collectPoint := func(humanFriendlyQueryConditions string, tags map[string]interface{}, ll LatLon) error {
		pt, err := getPoint(tags, ll)
		if err != nil {
			return fmt.Errorf("getting point for tags(%v) and LatLon(%v): %w", tags, ll, err)
		}
		pois[pt] = struct{}{}
		// always mark because we want to stats to represent the raw number of points returned by each query.
		// we could mark separately the number of points that come up duplicated across multiple segements/splits
		// but I'm not sure that's worth it, tbh.
		queryPointCounts.mark(humanFriendlyQueryConditions)
		return nil
	}

	executeQuery := elementQuerier()
	for splitI, split := range splits {
		for i, query := range queries {
			humanFriendlyQueryConditions := fmt.Sprintf("%+v", query.conditions)
			locus := 80
			// check not negative, could also memoize
			if query.radius != 0 {
				locus = query.radius
			}
			aroundRoute, err := queryRouteComponent(locus, split)
			if err != nil {
				return fmt.Errorf("creating query route component: %w", err)
			}

			log.Println("Split", splitI+1, "of", len(splits), "executing query", i+1, "of", len(queries))
			nodes, err := nodes(executeQuery, query.conditions, aroundRoute)
			if err != nil {
				return fmt.Errorf("getting nodes: %w", err)
			}
			log.Println("Retrieved nodes:", len(nodes))
			for _, node := range nodes {
				if err := collectPoint(humanFriendlyQueryConditions, node.Tags, LatLon{
					Lat: node.Lat,
					Lon: node.Lon,
				}); err != nil {
					return fmt.Errorf("collecting point for node(%v): %w", node, err)
				}
			}
			log.Println("Total pois:", getStats(0).totalPoints)

			wayCentres, err := wayCentres(executeQuery, query.conditions, aroundRoute)
			if err != nil {
				return fmt.Errorf("getting way centres: %w", err)
			}
			log.Println("Retrieved way centres:", len(wayCentres))

			for _, wayCentre := range wayCentres {
				if err := collectPoint(humanFriendlyQueryConditions, wayCentre.Tags, wayCentre.Centre); err != nil {
					return fmt.Errorf("collecting point for wayCentre(%v): %w", wayCentre, err)
				}
			}
			log.Println("Total pois:", getStats(0).totalPoints)
		}
	}
	if err := writePois(pois, getStats); err != nil {
		return fmt.Errorf("writing pois: %w", err)
	}

	stats := getStats(20)
	for _, occurrence := range stats.tagOccurrences {
		log.Println("Tag:", occurrence.value, "=>", occurrence.freq)
	}
	for _, occurrence := range stats.tagValueOccurrences {
		log.Println("Tag-Value:", occurrence.value, "=>", occurrence.freq)
	}

	topQueryCounts := queryPointCounts.topK(20)
	for _, q := range topQueryCounts {
		log.Println("Query:", q.value, "=>", q.freq)
	}

	return nil
}

func writePois(pois map[Point]struct{}, getStats func(topK int) stats) error {
	sortedPOIs :=
		slices.SortedFunc((maps.Keys(pois)), func(i, j Point) int {
			if i == j {
				return 0
			}
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
			return cmp.Compare(i.Lon, j.Lon)
		})
	f, err := os.CreateTemp("", "pois-json")
	if err != nil {
		return fmt.Errorf("creating temp file for output: %w", err)
	}
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(sortedPOIs); err != nil {
		return fmt.Errorf("writing output json: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing output json file(%s): %w", f.Name(), err)
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

	log.Println("output:", f.Name(), "pois:", len(pois))
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

func point(namePrefix string) (func(tags map[string]interface{}, latLon LatLon) (Point, error), func(topK int) stats) {
	var totalPoints int
	tagOccurrences := make(occurrences[string])
	tagValueOccurrences := make(occurrences[string])
	return func(tags map[string]interface{}, latLon LatLon) (Point, error) {
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
				if tag == "waterway" {
					log.Printf("%+v", tags)
				}
			}
			nodePoint := Point{
				Name:        namePrefix + name,
				Lat:         latLon.Lat,
				Lon:         latLon.Lon,
				Description: string(desc),
				Symbol:      resolveSymbol(tags),
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

func queryRouteComponent(locus int, route []gpxgo.GPXPoint) (string, error) {
	if len(route) == 0 {
		return "", fmt.Errorf(`no route points provided`)
	}
	var sb strings.Builder
	if _, err := sb.WriteString(`(around:` + strconv.Itoa(locus) + `,`); err != nil {
		return "", fmt.Errorf(`writing query route component: %w`, err)
	}
	if _, err := sb.WriteString(strconv.FormatFloat(route[0].Latitude, 'f', 6, 64)); err != nil {
		return "", fmt.Errorf(`writing query route component: %w`, err)
	}
	if _, err := sb.WriteString(`,`); err != nil {
		return "", fmt.Errorf(`writing query route component: %w`, err)
	}
	if _, err := sb.WriteString(strconv.FormatFloat(route[0].Longitude, 'f', 6, 64)); err != nil {
		return "", fmt.Errorf(`writing query route component: %w`, err)
	}
	for _, p := range route[1:] {
		if _, err := sb.WriteString(`,`); err != nil {
			return "", fmt.Errorf(`writing query route component: %w`, err)
		}
		if _, err := sb.WriteString(strconv.FormatFloat(p.Latitude, 'f', 6, 64)); err != nil {
			return "", fmt.Errorf(`writing query route component: %w`, err)
		}
		if _, err := sb.WriteString(`,`); err != nil {
			return "", fmt.Errorf(`writing query route component: %w`, err)
		}
		if _, err := sb.WriteString(strconv.FormatFloat(p.Longitude, 'f', 6, 64)); err != nil {
			return "", fmt.Errorf(`writing query route component: %w`, err)
		}
	}
	if _, err := sb.WriteString(`);
(._;>;);
out meta;`); err != nil {
		return "", fmt.Errorf(`writing query route component: %w`, err)
	}
	return sb.String(), nil
}

func nodes(makeQuery func(string, []condition, string) ([]element, error), conditions []condition, aroundRoute string) ([]element, error) {
	responseElements, err := makeQuery(`node`, conditions, aroundRoute)
	if err != nil {
		return nil, fmt.Errorf("getting query response elements: %w", err)
	}

	for _, e := range responseElements {
		if e.Type != `node` {
			return nil, fmt.Errorf(`node query response returned non-node type element: %v`, e)
		}
	}

	return responseElements, nil
}

func wayCentres(makeQuery func(string, []condition, string) ([]element, error), conditions []condition, route string) ([]wayCentre, error) {
	responseElements, err := makeQuery(`way`, conditions, route)
	if err != nil {
		return nil, fmt.Errorf("getting query response elements: %w", err)
	}

	nodes := make(map[int64]element)
	ways := make(map[int64]element)
	for _, e := range responseElements {
		switch e.Type {
		case `node`:
			nodes[e.ID] = e
		case `way`:
			ways[e.ID] = e
		default:
			return nil, fmt.Errorf("unknown element type: %s: %v", e.Type, e)
		}
	}

	wayCentres := make([]wayCentre, 0, len(ways))
	for _, way := range ways {
		if len(way.Nodes) == 0 {
			return nil, fmt.Errorf("no nodes for way %d", way.ID)
		}
		var centre LatLon
		for _, nodeID := range way.Nodes {
			node, ok := nodes[nodeID]
			if !ok {
				return nil, fmt.Errorf("node %d not found", nodeID)
			}
			centre.Lat += node.Lat / float64(len(way.Nodes))
			centre.Lon += node.Lon / float64(len(way.Nodes))
		}
		wayCentres = append(wayCentres, wayCentre{
			ID:     way.ID,
			Centre: centre,
			Tags:   way.Tags,
		})
	}

	return wayCentres, nil
}

func elementQuerier() func(queryType string, queryConditions []condition, route string) ([]element, error) {
	return func(queryType string, queryConditions []condition, route string) ([]element, error) {
		humanFriendlyQueryConditions := fmt.Sprintf("%+v", queryConditions)
		renderedQuery, elements, err := buildQuery(queryType, queryConditions, route)
		if err != nil {
			return elements, err
		}
		if debug {
			log.Printf("query (%s) build from conditions (%+v) and type (%s)", renderedQuery, queryConditions, queryType)
		}
		hasher := sha1.New()
		if _, err := hasher.Write([]byte(renderedQuery)); err != nil {
			return nil, fmt.Errorf("hashing query: %w", err)
		}
		sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))

		var rc io.ReadCloser
		queryStateFilePath := filepath.Join(dataDir, sha)
		if stored, err := os.Open(queryStateFilePath); err == nil {
			if debug {
				log.Printf("query fetched from cached result: %s", queryStateFilePath)
			}
			rc = stored
		} else if os.IsNotExist(err) {
			log.Printf("query result not cached, making query to API: %s", humanFriendlyQueryConditions)
			resp, err := doQuery(renderedQuery)
			if err != nil {
				return nil, err
			}
			file, err := os.OpenFile(queryStateFilePath, os.O_RDWR|os.O_CREATE, 0666)
			if err != nil {
				_ = resp.Close()
				return nil, fmt.Errorf("opening query file for writing(%s): %w", humanFriendlyQueryConditions, err)
			}
			// TODO(glynternet): can we use a TeeReader here instead?
			if _, err := io.Copy(file, resp); err != nil {
				_ = resp.Close()
				return nil, fmt.Errorf("outputing response: %w", err)
			}
			if debug {
				log.Printf("query result written: %s", file.Name())
			}
			if err := resp.Close(); err != nil {
				return nil, fmt.Errorf("closing response: %w", err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("seeking to start of query response state file: %w", err)
			}
			rc = file
		} else if err != nil {
			return nil, fmt.Errorf("opening query state file(%s): %w", queryStateFilePath, err)
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
}

func buildQuery(queryType string, queryConditions []condition, route string) (string, []element, error) {
	var sb strings.Builder
	sb.WriteString(`[out:json];` + queryType)
	for _, element := range queryConditions {
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
			return "", nil, fmt.Errorf("query element must contain only one condition: 'not', 'values' or 'exists': %+v", element)
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
				return "", nil, fmt.Errorf("unsupported exists value: %+v", element.exists)
			}
		default:
			return "", nil, fmt.Errorf("query element contains no conditions: %+v", element)
		}
		for _, elementCondition := range elementConditions {
			if _, err := sb.WriteString(`[` + elementCondition + `]`); err != nil {
				return "", nil, fmt.Errorf("writing query element: %w", err)
			}
		}
	}
	sb.WriteString(route)

	renderedQuery := sb.String()
	return renderedQuery, nil, nil
}

func doQuery(renderedQuery string) (io.ReadCloser, error) {
	var resp *http.Response
	for attempt := 1; ; attempt++ {
		// curl -d @<(cat <(echo "[out:json];$type$params") ~/tmp/pois/query_end) -X POST http://overpass-api.de/api/interpreter
		var err error
		resp, err = http.Post(`http://overpass-api.de/api/interpreter`, "", strings.NewReader(renderedQuery))
		if err == nil {
			switch resp.StatusCode {
			case http.StatusOK:
				return resp.Body, nil
			case http.StatusTooManyRequests:
				err = errors.New("rate limited")
			default:
				_, _ = io.Copy(os.Stderr, resp.Body)
				_ = resp.Body.Close()
				return nil, fmt.Errorf("posting query (%s): unexpected status code %d (%s)", renderedQuery, resp.StatusCode, resp.Status)
			}
		}
		const maxAttempts = 50
		if attempt >= maxAttempts {
			return nil, fmt.Errorf("posting query(%s), too many retries: %w", renderedQuery, err)
		}
		waitDuration := time.Second * time.Duration(float64(2)*math.Pow(1.5, 1.0+float64(attempt)/float64(maxAttempts)))
		log.Printf("Error executing query (attempt %d/%d), will retry in %s: %s", attempt, maxAttempts, waitDuration.String(), err.Error())
		time.Sleep(waitDuration)
	}
}

func resolveName(tags map[string]interface{}) (string, error) {
	for _, tag := range []string{
		"name",
		"amenity",
		"tourism",
		"leisure",
		"shop",
		"waterway",
		"natural",
		"boundary", // probably good to combine this with another tag
		"man_made",
	} {
		n, ok := tags[tag].(string)
		if ok {
			return n, nil
		}
	}
	return "", errors.New("no suitable tag for name")
}

func resolveSymbol(tags map[string]interface{}) string {
	var symbol string
	for _, symbolMatchers := range []struct {
		tags   map[string]string
		symbol string
	}{
		{tags: map[string]string{"shop": "convenience"}, symbol: "Shopping Center"},
		{tags: map[string]string{"shop": "supermarket"}, symbol: "Shopping Center"},
		{tags: map[string]string{"leisure": "park"}, symbol: "Park"},
		{tags: map[string]string{"boundary": "protected_area"}, symbol: "Park"},
		{tags: map[string]string{"amenity": "toilets"}, symbol: "Restroom"},
		{tags: map[string]string{"amenity": "drinking_water"}, symbol: "Drinking Water"},
		{tags: map[string]string{"natural": "peak"}, symbol: "Summit"},
		{tags: map[string]string{"natural": "saddle"}, symbol: "Summit"},
		{tags: map[string]string{"mountain_pass": "yes"}, symbol: "Summit"},
		{tags: map[string]string{"tourism": "viewpoint"}, symbol: "Scenic Area"},
		{tags: map[string]string{"amenity": "bicycle_repair_station"}, symbol: "Mine"},
		{tags: map[string]string{"amenity": "fast_food"}, symbol: "Fast Food"},
		{tags: map[string]string{"amenity": "fuel"}, symbol: "Gas Station"},
		{tags: map[string]string{"amenity": "pub"}, symbol: "Bar"},
		{tags: map[string]string{"amenity": "bar"}, symbol: "Bar"},
		{tags: map[string]string{"amenity": "cafe"}, symbol: "Restaurant"},
		{tags: map[string]string{"tourism": "picnic_site"}, symbol: "Picnic Area"},
		{tags: map[string]string{"amenity": "restaurant", "cuisine": "pizza"}, symbol: "Pizza"},
		{tags: map[string]string{"amenity": "restaurant"}, symbol: "Restaurant"},
		{tags: map[string]string{"amenity": "ice_cream"}, symbol: "Fast Food"},
		{tags: map[string]string{"tourism": "camp_pitch"}, symbol: "Campground"},
		{tags: map[string]string{"tourism": "camp_site"}, symbol: "Campground"},
		{tags: map[string]string{"leisure": "nature_reserve"}, symbol: "Park"},
		{tags: map[string]string{"amenity": "shelter"}, symbol: "Building"},
		{tags: map[string]string{"amenity": "place_of_worship"}, symbol: "Church"},
		{tags: map[string]string{"place": "town"}, symbol: "City Hall"},
		{tags: map[string]string{"place": "village"}, symbol: "City Hall"},
		{tags: map[string]string{"place": "hamlet"}, symbol: "City Hall"},
		{tags: map[string]string{"place": "city"}, symbol: "City Hall"},
		{tags: map[string]string{"place": "neighbourhood"}, symbol: "City Hall"},
		{tags: map[string]string{"waterway": "river"}, symbol: "Water Source"},
	} {
		match := true
		for k, matcherV := range symbolMatchers.tags {
			v, ok := tags[k]
			if !ok || v != matcherV {
				match = false
				break
			}
		}
		if match {
			symbol = symbolMatchers.symbol
			break
		}
	}
	return symbol
}
