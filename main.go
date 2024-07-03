package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	gpxgo "github.com/tkrajina/gpxgo/gpx"
)

const (
	dataDir = `/tmp/route-poi-finder-state`

	split = 1
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
	distance  int
	values    []string
	notValues []string
	exists    int
}

var queries = []query{{
	conditions: []condition{{
		tag: "amenity",
		values: []string{
			"bar",
			"bicycle_rental",
			"bicycle_repair_station",
			"bicycle_wash",
			"biergarten",
			"cafe",
			"compressed_air",
			"fast_food",
			"food_court",
			"fountain",
			"fuel",
			"ice_cream",
			"marketplace",
			"place_of_worship",
			"pub",
			"public_bath",
			"restaurant",
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
	conditions: []condition{{
		// - - tourism~"^(alpine_hut|camp_pitch|camp_site|guest_house|hostel|picnic_site|viewpoint|wilderness_hut)$"
		tag: "tourism",
		values: []string{
			"alpine_hut",
			"camp_pitch",
			"camp_site",
			"guest_house",
			"hostel",
			"picnic_site",
			"viewpoint",
			"wilderness_hut",
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
		// boundary=aboriginal_lands
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
	radius: 200,
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
	radius: 200,
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
	if len(os.Args) < 2 {
		log.Println("must provide args")
		os.Exit(1)
	}
	if err := mainErr(os.Args[1]); err != nil {
		log.Println(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func mainErr(file string) error {
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
	chunkSize := len(pts) / split
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

	for _, split := range splits {
		getPoint, getStats := point()
		var pois []Point
		for i, query := range queries {
			locus := 80
			// check not negative, could also memoize
			if query.radius != 0 {
				locus = query.radius
			}
			aroundRoute, err := queryRouteComponent(locus, split)
			if err != nil {
				return fmt.Errorf("creating query route component: %w", err)
			}

			log.Println("Executing query", i, "of", len(queries))
			nodes, err := nodes(query, aroundRoute)
			if err != nil {
				return fmt.Errorf("getting nodes: %w", err)
			}
			log.Println("Retrieved nodes:", len(nodes))
			for _, node := range nodes {
				pt, err := getPoint(node.Tags, LatLon{
					Lat: node.Lat,
					Lon: node.Lon,
				})
				if err != nil {
					return fmt.Errorf("getting point for node(%v): %w", node, err)
				}
				pois = append(pois, pt)
			}
			log.Println("Total pois:", getStats(0).totalPoints)

			wayCentres, err := wayCentres(query.conditions, aroundRoute)
			if err != nil {
				return fmt.Errorf("getting way centres: %w", err)
			}
			log.Println("Retrieved way centres:", len(wayCentres))

			for _, wayCentre := range wayCentres {
				pt, err := getPoint(wayCentre.Tags, wayCentre.Centre)
				if err != nil {
					return fmt.Errorf("getting point for way(%v): %w", wayCentre, err)
				}
				pois = append(pois, pt)
			}
			log.Println("Total pois:", getStats(0).totalPoints)
		}
		sort.Slice(pois, func(i, j int) bool {
			if pois[i] == pois[j] {
				return false
			}
			if pois[i].Name != pois[j].Name {
				return pois[i].Name < pois[j].Name
			}
			if pois[i].Description != pois[j].Description {
				return pois[i].Description < pois[j].Description
			}
			if pois[i].Symbol != pois[j].Symbol {
				return pois[i].Symbol < pois[j].Symbol
			}
			if pois[i].Lat != pois[j].Lat {
				return pois[i].Lat < pois[j].Lat
			}
			return pois[i].Lon < pois[j].Lon
		})
		f, err = os.CreateTemp("", "pois-json")
		if err != nil {
			return fmt.Errorf("creating temp file for output: %w", err)
		}
		encoder := json.NewEncoder(f)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(pois); err != nil {
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
	}

	return nil
}

type stats struct {
	totalPoints         int
	tagOccurrences      []valueFreq
	tagValueOccurrences []valueFreq
}

type valueFreq struct {
	value string
	freq  int
}

func point() (func(tags map[string]interface{}, latLon LatLon) (Point, error), func(topK int) stats) {
	var totalPoints int
	tagOccurrences := make(map[string]int)
	tagValueOccurrences := make(map[string]int)
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
				tagOccurrences[tag]++
				tagValueOccurrences[tag+":"+value.(string)]++
			}
			nodePoint := Point{
				Name:        name,
				Lat:         latLon.Lat,
				Lon:         latLon.Lon,
				Description: string(desc),
				Symbol:      resolveSymbol(tags),
			}
			totalPoints++
			return nodePoint, nil
		}, func(topK int) stats {
			var outputTagOccurrences []valueFreq
			for tag, freq := range tagOccurrences {
				outputTagOccurrences = append(outputTagOccurrences, valueFreq{
					value: tag,
					freq:  freq,
				})
			}
			sort.Slice(outputTagOccurrences, func(i, j int) bool {
				// descending
				return outputTagOccurrences[i].freq > outputTagOccurrences[j].freq
			})
			if len(outputTagOccurrences) > topK {
				outputTagOccurrences = outputTagOccurrences[:topK]
			}
			var outputTagValueOccurrences []valueFreq
			for tagValue, freq := range tagValueOccurrences {
				outputTagValueOccurrences = append(outputTagValueOccurrences, valueFreq{
					value: tagValue,
					freq:  freq,
				})
			}
			sort.Slice(outputTagValueOccurrences, func(i, j int) bool {
				return outputTagValueOccurrences[i].freq > outputTagValueOccurrences[j].freq
			})
			// descending
			if len(outputTagValueOccurrences) > topK {
				outputTagValueOccurrences = outputTagValueOccurrences[:topK]
			}
			return stats{
				totalPoints:         totalPoints,
				tagOccurrences:      outputTagOccurrences,
				tagValueOccurrences: outputTagValueOccurrences,
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

func nodes(elements query, aroundRoute string) ([]element, error) {
	responseElements, err := queryResponseElements(`node`, elements.conditions, aroundRoute)
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

func wayCentres(conditions []condition, route string) ([]wayCentre, error) {
	responseElements, err := queryResponseElements(`way`, conditions, route)
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

func queryResponseElements(queryType string, queryConditions []condition, route string) ([]element, error) {
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
			return nil, fmt.Errorf("query element must contain only one condition: 'not', 'values' or 'exists': %+v", element)
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
				return nil, fmt.Errorf("unsupported exists value: %+v", element.exists)
			}
		default:
			return nil, fmt.Errorf("query element contains no conditions: %+v", element)
		}
		for _, elementCondition := range elementConditions {
			if _, err := sb.WriteString(`[` + elementCondition + `]`); err != nil {
				return nil, fmt.Errorf("writing query element: %w", err)
			}
		}
	}
	sb.WriteString(route)

	renderedQuery := sb.String()
	hasher := sha1.New()
	if _, err := hasher.Write([]byte(renderedQuery)); err != nil {
		return nil, fmt.Errorf("hashing query: %w", err)
	}
	sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))

	var rc io.ReadCloser
	queryStateFilePath := filepath.Join(dataDir, sha)
	if stored, err := os.Open(queryStateFilePath); err == nil {
		log.Printf("query fetched from cached result: %s", queryStateFilePath)
		rc = stored
	} else if os.IsNotExist(err) {
		log.Printf("query result not cached, making query to API: %s:%+v", queryType, queryConditions)
		// curl -d @<(cat <(echo "[out:json];$type$params") ~/tmp/pois/query_end) -X POST http://overpass-api.de/api/interpreter
		resp, err := http.Post(`http://overpass-api.de/api/interpreter`, "", strings.NewReader(renderedQuery))
		if err != nil {
			return nil, fmt.Errorf("posting query(%+v): %w", queryConditions, err)
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(os.Stderr, resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("posting query (%s:%+v): unexpected status code %d (%s)", queryType, queryConditions, resp.StatusCode, resp.Status)
		}
		file, err := os.OpenFile(queryStateFilePath, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			return nil, fmt.Errorf("opening query file for writing(%+v): %w", queryConditions, err)
		}
		if _, err := io.Copy(file, resp.Body); err != nil {
			return nil, fmt.Errorf("outputing response body: %w", err)
		}
		log.Printf("query result written: %s", file.Name())
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("closing response body: %w", err)
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

func resolveName(tags map[string]interface{}) (string, error) {
	for _, tag := range []string{
		"name",
		"amenity",
		"tourism",
		"leisure",
		"shop",
		"waterway",
		"natural",
		// probably good to combine this with another tag
		"boundary",
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
