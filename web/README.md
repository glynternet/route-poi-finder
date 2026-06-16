# POI triage UI

A single static page for visually triaging `route-poi-finder` output on a map: load
the POI JSON, draw areas to include/exclude, then download the pruned set in the **same
JSON format** so it drops straight back into your workflow.

## Usage

Open `index.html` directly in a browser, or serve the directory:

```bash
python3 -m http.server -d web 8000
# then visit http://localhost:8000
```

(Map tiles and the Leaflet libraries load from the internet via CDN.)

Then:

1. **Load POI JSON** — drag-and-drop the `route-poi-finder` output `.json` onto the page
   (or use the *Load POI JSON* button). Markers are plotted and the map fits to them.
2. **Load route GPX** *(optional)* — drop the original `.gpx` to draw the route line
   underneath the POIs for context.
3. **Draw areas** — pick a draw mode (**Exclude** or **Include**), then use the
   rectangle/polygon tools on the map:
   - **Exclude** removes POIs inside the shape.
   - **Include** — if any include area exists, only POIs inside an include area are kept
     (minus anything excluded). With no include areas, everything is kept except
     excludes.
   - Edit/delete shapes via the draw toolbar or the **Areas** list. Counts update live.
4. **Tag filters** — prune by OSM tags instead of (or as well as) geography:
   - Pick a mode (**Exclude** / **Include**), choose a tag **key** (and optionally a
     specific **value**, or leave it as *(any value)*), then **Add**. Keys and values are
     drawn from the loaded POIs and annotated with how many carry them.
   - **Exclude** drops POIs whose tags match; if any **Include** filter exists, only POIs
     matching an include filter are kept. Each filter shows a live match count; click its
     `excl`/`incl` chip to flip the mode, or ✕ to remove it.
   - Tag filters apply **on top of (AND with)** the area filters — a POI is kept only when
     it survives both.
5. **Download filtered JSON** — exports the kept POIs as `pois-filtered.json`, identical
   in shape (`name, lat, lon, desc, sym, categories, osmid, tags`) to the input.

## Generating input

```bash
go run main.go --split 2 <route.gpx> > pois.json
```

No build step, no dependencies to install — it's one HTML file.
