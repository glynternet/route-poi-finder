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
	stateHome = `/home/g/tmp/route-poi-finder-state`

	gpxFile = `/home/g/Downloads/2021-11-27_580704200_North_South_CO_2023.gpx`

	split = 10
)

const (
	// TODO(glynternet): workout better API
	ExistsUndefined = iota
	ExistsYes
	ExistsNo
)

type query []struct {
	tag    string
	values []string
	not    string
	exists int
}

var queries = []query{{{
	// - - amenity~"^(bar|biergarten|cafe|fast_food|food_court|fuel|ice_cream|pub|restaurant|bicycle_repair_station|compressed_air|drinking_water|shelter|toilets|water_point|marketplace|place_of_worship)$"
	tag: "amenity",
	values: []string{
		"bar",
		"bicycle_rental",
		"bicycle_repair_station",
		"bicycle_wash",
		"biergarten",
		"cafe",
		"compressed_air",
		"drinking_water",
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
		"water_point",
		"watering_place",
	},
}}, {{
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
}}, {{
	// - - leisure~"^(nature_reserve|park|picnic_table|wildlife_hide)$"
	tag: "leisure",
	values: []string{
		"nature_reserve",
		"park",
		"picnic_table",
		"wildlife_hide",
	},
}}, {{
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
}}, {{
	// - - man_made~"^(spring_box|water_well|water_tap)$"
	tag: "man_made",
	values: []string{
		"spring_box",
		"water_well",
		"water_tap",
		"drinking_fountain",
	},
}}, {{
	tag: "drinking_water",
	values: []string{
		"yes",
	},
}}, {{
	tag:    "waterway",
	exists: ExistsYes,
}}, {{
	// - - place~"^(town|village|hamlet|city|neighbourhood)$"
	tag: "place",
	values: []string{
		"town",
		"village",
		"hamlet",
		"city",
		"neighbourhood",
	},
}}, {
	//- - amenity="fountain"
	//  - drinking_water!="no"
	//  - drinking_water~".+"
	{
		tag:    "amenity",
		values: []string{"fountain"},
	}, {
		tag:    "drinking_water",
		values: []string{".+"},
	}, {
		tag: "drinking_water",
		not: "no",
	},
}, {{
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
}}, {{
	tag:    "mountain_pass",
	values: []string{"yes"},
}}}

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
	log.SetFlags(log.Lshortfile)
	if err := mainErr(); err != nil {
		log.Println(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func mainErr() error {
	f, err := os.Open(gpxFile)
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

	pts := gpx.Tracks[0].Segments[0].Points

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
	var splitPoints int64
	for _, points := range splits {
		splitPoints += int64(len(points))
	}
	log.Println("splits:", len(splits), "points:", splitPoints)

	var pois []Point
	for _, split := range splits {
		aroundRoute, err := queryRouteComponent(80, split)
		if err != nil {
			return fmt.Errorf("creating query route component: %w", err)
		}

		for _, elements := range queries {
			nodes, err := nodes(elements, aroundRoute)
			if err != nil {
				return fmt.Errorf("getting nodes: %w", err)
			}

			for _, node := range nodes {
				pt, err := point(node.Tags, LatLon{
					Lat: node.Lat,
					Lon: node.Lon,
				})
				if err != nil {
					return fmt.Errorf("getting point for node(%v): %w", node, err)
				}
				pois = append(pois, pt)
			}
			wayCentres, err := wayCentres(elements, aroundRoute)
			if err != nil {
				return fmt.Errorf("getting way centres: %w", err)
			}
			for _, wayCentre := range wayCentres {
				pt, err := point(wayCentre.Tags, wayCentre.Centre)
				if err != nil {
					return fmt.Errorf("getting point for way(%v): %w", wayCentre, err)
				}
				pois = append(pois, pt)
			}
		}
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
	log.Println("output:", f.Name(), "points:", len(pois))
	return nil
}

func point(tags map[string]interface{}, latLon LatLon) (Point, error) {
	name, err := resolveName(tags)
	if err != nil {
		return Point{}, fmt.Errorf("resolving name from tags(%v): %w", tags, err)
	}
	desc, err := json.Marshal(tags)
	if err != nil {
		return Point{}, fmt.Errorf("marshalling node tags for description: %w", err)
	}
	nodePoint := Point{
		Name:        name,
		Lat:         latLon.Lat,
		Lon:         latLon.Lon,
		Description: string(desc),
		Symbol:      resolveSymbol(tags),
	}
	return nodePoint, nil
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
	responseElements, err := queryResponseElements(`node`, elements, aroundRoute)
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

func wayCentres(elements query, route string) ([]wayCentre, error) {
	responseElements, err := queryResponseElements(`way`, elements, route)
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

func queryResponseElements(queryType string, query query, route string) ([]element, error) {
	var sb strings.Builder
	sb.WriteString(`[out:json];` + queryType)
	for _, element := range query {
		var definedConditions int
		for _, condition := range []bool{
			element.not != "",
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
		var elementCondition string
		switch {
		case len(element.values) > 0:
			elementCondition = fmt.Sprintf(`%s~"^(%s)$"`, element.tag, strings.Join(element.values, "|"))
		case element.not != "":
			elementCondition = fmt.Sprintf(`%s!="%s"`, element.tag, element.not)
		case element.exists != ExistsUndefined:
			switch element.exists {
			case ExistsYes:
				elementCondition = fmt.Sprintf(`%s`, element.tag)
			case ExistsNo:
				elementCondition = fmt.Sprintf(`!%s`, element.tag)
			default:
				return nil, fmt.Errorf("unsupported exists value: %+v", element.exists)
			}
		default:
			return nil, fmt.Errorf("query element contains no conditions: %+v", element)
		}
		if _, err := sb.WriteString(`[` + elementCondition + `]`); err != nil {
			return nil, fmt.Errorf("writing query element: %w", err)
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
	queryStateFilePath := filepath.Join(stateHome, sha)
	if stored, err := os.Open(queryStateFilePath); err == nil {
		log.Printf("query fetched from cached result: %s", queryStateFilePath)
		rc = stored
	} else if os.IsNotExist(err) {
		log.Printf("query result not cached, making query to API: %s:%+v", queryType, query)
		// curl -d @<(cat <(echo "[out:json];$type$params") ~/tmp/pois/query_end) -X POST http://overpass-api.de/api/interpreter
		resp, err := http.Post(`http://overpass-api.de/api/interpreter`, "", strings.NewReader(renderedQuery))
		if err != nil {
			return nil, fmt.Errorf("posting query(%+v): %w", query, err)
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(os.Stderr, resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("posting query (%s:%s): unexpected status code %d (%s)", queryType, query, resp.StatusCode, resp.Status)
		}
		file, err := os.OpenFile(queryStateFilePath, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			return nil, fmt.Errorf("opening query file for writing(%+v): %w", query, err)
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
		{tags: map[string]string{"leisure": "park"}, symbol: "Park"},
		{tags: map[string]string{"amenity": "toilets"}, symbol: "Restroom"},
		{tags: map[string]string{"amenity": "drinking_water"}, symbol: "Drinking Water"},
		{tags: map[string]string{"natural": "peak"}, symbol: "Summit"},
		{tags: map[string]string{"tourism": "viewpoint"}, symbol: "Scenic Area"},
		{tags: map[string]string{"amenity": "bicycle_repair_station"}, symbol: "Mine"},
		{tags: map[string]string{"amenity": "fast_food"}, symbol: "Fast Food"},
		{tags: map[string]string{"amenity": "fuel"}, symbol: "Gas Station"},
		{tags: map[string]string{"amenity": "pub"}, symbol: "Bar"},
		{tags: map[string]string{"amenity": "cafe"}, symbol: "Restaurant"},
		{tags: map[string]string{"tourism": "picnic_site"}, symbol: "Picnic Area"},
		{tags: map[string]string{"amenity": "restaurant", "cuisine": "pizza"}, symbol: "Pizza"},
		{tags: map[string]string{"amenity": "restaurant"}, symbol: "Restaurant"},
		{tags: map[string]string{"amenity": "ice_cream"}, symbol: "Fast Food"},
		{tags: map[string]string{"tourism": "camp_pitch"}, symbol: "Campground"},
		{tags: map[string]string{"leisure": "nature_reserve"}, symbol: "Park"},
		{tags: map[string]string{"amenity": "shelter"}, symbol: "Building"},
		{tags: map[string]string{"amenity": "place_of_worship"}, symbol: "Church"},
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
