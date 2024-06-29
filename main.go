package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
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

	queriesCfg = `# Need to get nodes and ways
- - amenity~"^(bar|biergarten|cafe|fast_food|food_court|fuel|ice_cream|pub|restaurant|bicycle_repair_station|compressed_air|drinking_water|shelter|toilets|water_point|marketplace|place_of_worship)$"
- - tourism~"^(alpine_hut|camp_pitch|camp_site|guest_house|hostel|picnic_site|viewpoint|wilderness_hut)$"
- - amenity="fountain"
  - drinking_water!="no"
  - drinking_water~".+"
- - leisure~"^(nature_reserve|park|picnic_table|wildlife_hide)$"
- - natural~"^(spring|peak)$"
- - man_made~"^(spring_box|water_well|water_tap)$"`

	gpxFile = `/home/g/Downloads/2021-11-27_580704200_North_South_CO_2023.gpx`

	split = 10
)

// API

type query []string

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

	var queries []query
	if err := yaml.Unmarshal([]byte(queriesCfg), &queries); err != nil {
		return fmt.Errorf("unmarshalling config: %w", err)
	}

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
	log.Println("output:", f.Name())
	return nil
}

func point(tags map[string]interface{}, latLon LatLon) (Point, error) {
	name, err := resolveName(tags)
	if err != nil {
		return Point{}, fmt.Errorf("resolving node name from tags(%v): %w", tags, err)
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

	sort.Slice(responseElements, func(i, j int) bool {
		return responseElements[i].ID < responseElements[j].ID
	})

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

	sort.Slice(wayCentres, func(i, j int) bool {
		return wayCentres[i].ID < wayCentres[j].ID
	})

	return wayCentres, nil
}

func queryResponseElements(queryType string, elements query, route string) ([]element, error) {
	var sb strings.Builder
	sb.WriteString(`[out:json];` + queryType)
	for _, element := range elements {
		if _, err := sb.WriteString(`[` + element + `]`); err != nil {
			return nil, fmt.Errorf("writing query element: %w", err)
		}
	}
	sb.WriteString(route)

	query := sb.String()
	hasher := sha1.New()
	if _, err := hasher.Write([]byte(query)); err != nil {
		return nil, fmt.Errorf("hashing query: %w", err)
	}
	sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))

	var rc io.ReadCloser
	queryStateFilePath := filepath.Join(stateHome, sha)
	if stored, err := os.Open(queryStateFilePath); err == nil {
		log.Printf("query fetched from cached result: %s", queryStateFilePath)
		rc = stored
	} else if os.IsNotExist(err) {
		log.Printf("query result not cached, making query to API: %s:%s", queryType, elements)
		// curl -d @<(cat <(echo "[out:json];$type$params") ~/tmp/pois/query_end) -X POST http://overpass-api.de/api/interpreter
		resp, err := http.Post(`http://overpass-api.de/api/interpreter`, "", strings.NewReader(query))
		if err != nil {
			return nil, fmt.Errorf("posting query(%s): %w", elements, err)
		}
		file, err := os.OpenFile(queryStateFilePath, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			return nil, fmt.Errorf("opening query file for writing(%s): %w", elements, err)
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
		return nil, fmt.Errorf("decoding response body: %w", err)
	}

	return r.Elements, nil
}

func resolveName(tags map[string]interface{}) (string, error) {
	if n, ok := tags["name"]; ok {
		return n.(string), nil
	} else if a, ok := tags["amenity"]; ok {
		return a.(string), nil
	} else if a, ok := tags["tourism"]; ok {
		return a.(string), nil
	} else if a, ok := tags["leisure"]; ok {
		return a.(string), nil
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
