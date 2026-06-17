# POI triage UI

A single static page for visually triaging `route-poi-finder` output on a map: load
the POI GeoJSON, draw areas to include/exclude, then download the pruned set in the **same
GeoJSON format** so it drops straight back into your workflow. POIs render as
category-coloured markers with a Maki/Temaki icon and a name label.

## Usage

Open `index.html` directly in a browser, or serve the directory:

```bash
python3 -m http.server -d web 8000
# then visit http://localhost:8000
```

(Map tiles, the Leaflet libraries, and the Maki/Temaki marker icons all load from the
internet via CDN.)

Then:

1. **Load POI GeoJSON** — drag-and-drop the `route-poi-finder` output `.geojson` onto the
   page (or use the *Load POI GeoJSON* button). Markers are plotted and the map fits to
   them. Toggle **Show name labels** to declutter when zoomed out.
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
5. **Download filtered GeoJSON** — exports the kept POIs as `pois-filtered.geojson`, a
   GeoJSON `FeatureCollection` identical in shape to the input (each feature has a
   namespaced `id`, `[lon, lat]` geometry, and `properties`: `name, category, categories,
   osm_type, osmid, tags`).

## Generating input

```bash
go run . --split 2 <route.gpx> > pois.geojson
```

No build step, no dependencies to install — it's one HTML file.
