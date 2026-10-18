# route-poi-finder

Find OpenStreetMap points of interest along a GPX route using Overpass. The input
must contain one track with one segment. Output is a GeoJSON `FeatureCollection`;
progress and diagnostics go to stderr.

## Build and run

```sh
make binaries
make installs                # install into GOBIN, or GOPATH/bin
route-poi-finder --help
route-poi-finder version
route-poi-finder --name-prefix 'Day1-' --split 10 route.gpx > pois.geojson

# Run from source without installing:
go run ./cmd/route-poi-finder --split 2 route.gpx > pois.geojson
```

Binaries are built under `build/<git-version>/linux-amd64/` by default. Set `OS`
and `ARCH` for another target. `make binaries` and `make installs` embed the Git
version; a plain `go run` or `go build` reports `unknown`.

The CLI uses Dubplate v0.15.0 and Cobra. Source commands now target
`./cmd/route-poi-finder` instead of `.`. Long flags use two dashes (`--split`),
and can appear before or after the GPX path. The existing flag names and defaults
are retained, including repeatable `--overpass-endpoint` values. Run `--help` for
the full list. `--out -` (the default) writes stdout, `--out PATH` writes/truncates
a file, and `--out ''` creates a temporary file.

Responses are cached in `$HOME/.route-poi-finder-state` for 28 days by default.
Use `--cache-ttl 0` to query fresh data, or `--cache-dir` to choose another cache.
Fully cached routes can run offline. Existing caches remain compatible.

## Development

```sh
make check                   # formatting, race tests, binaries and version check
go vet ./...
```

Tests use temporary data and local HTTP fixtures; they do not query live Overpass
servers. CLI tests cover argument validation, help/version, repeatable endpoints,
output destinations, redirected logging, cache replay and forced refresh.

Application code and tests live in `cmd/route-poi-finder/`. The Overpass HTTP
client remains in `overpass/`. `main.go` is generated; implement command behavior
in `cmdtree.go` and processing in the other Go files.

## Refreshing Dubplate

`dubplate.yml` declares the module and executable. From a clean Dubplate v0.15.0
checkout, preview and apply the generator (requires Go and kislyuk/yq):

```sh
./install.sh --dry-run /path/to/route-poi-finder /path/to/route-poi-finder/dubplate.yml
./install.sh /path/to/route-poi-finder /path/to/route-poi-finder/dubplate.yml
cd /path/to/route-poi-finder
go mod tidy
make check
```

Dubplate owns `cmd/route-poi-finder/main.go`, `dubplate.Makefile`, `go.Makefile`,
`docker.Makefile`, `Dockerfile.template` and `.github/workflows/go.yml`; do not
edit those generated files. The top-level Makefile, command tree and
`.github/workflows/main.yml` belong to this application. The latter runs checks
on pushes to `main`, alongside Dubplate's checks for pull requests and `develop`.
The generated Docker files are available but are not enabled by the Makefile.
The existing `.gitignore` is preserved by the generator; `/build/` is ignored
here explicitly.
