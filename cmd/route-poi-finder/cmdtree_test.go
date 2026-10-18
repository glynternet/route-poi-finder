package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	pkgcmd "github.com/glynternet/pkg/cmd"
	"github.com/glynternet/pkg/log"
	"github.com/spf13/cobra"
)

func executeCLI(args ...string) (string, string, error) {
	var out, diagnostics bytes.Buffer
	root := &cobra.Command{Use: appName}
	root.AddCommand(pkgcmd.NewVersionCmd("test-version", &out))
	buildCmdTree(log.NewLogger(&diagnostics), &out, root)
	root.SetErr(&diagnostics)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), diagnostics.String(), err
}

func TestCLIValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "accepts 1 arg(s), received 0"},
		{[]string{"one.gpx", "two.gpx"}, "accepts 1 arg(s), received 2"},
		{[]string{"--workers=-1", "route.gpx"}, "--workers must be at least 0"},
		{[]string{"--retries=-1", "route.gpx"}, "--retries must be at least 0"},
		{[]string{"--split=0", "route.gpx"}, "--split must be greater than 0"},
		{[]string{"--split=-1", "route.gpx"}, "invalid argument"},
		{[]string{"--cache-ttl=bad", "route.gpx"}, "invalid argument"},
		{[]string{"--unknown", "route.gpx"}, "unknown flag"},
		{[]string{"--overpass-endpoint=bad", "route.gpx"}, "expected NAME="},
		{[]string{"--overpass-endpoint=test=http://localhost,http://localhost,0", "route.gpx"}, "invalid concurrency"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, _, err := executeCLI(tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, want %q", err, tc.want)
			}
			if out != "" {
				t.Fatalf("invalid input wrote to stdout: %q", out)
			}
		})
	}
}

func TestCLIHelpAndVersion(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want string
	}{
		{"--help", "Find points of interest along a GPX route"},
		{"version", "test-version"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			out, diagnostics, err := executeCLI(tc.arg)
			if err != nil || !strings.Contains(out, tc.want) || diagnostics != "" {
				t.Fatalf("stdout=%q diagnostics=%q error=%v", out, diagnostics, err)
			}
		})
	}
}

func TestCLIOutputsAndCache(t *testing.T) {
	dir := t.TempDir()
	route := filepath.Join(dir, "route.gpx")
	if err := os.WriteFile(route, []byte(`<gpx version="1.1" creator="test"><trk><trkseg>
<trkpt lat="40" lon="-105"/><trkpt lat="40.1" lon="-105.1"/><trkpt lat="40.2" lon="-105.2"/>
</trkseg></trk></gpx>`), 0600); err != nil {
		t.Fatal(err)
	}
	var queryRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := "Rate limit: 0\n"
		switch r.URL.Path {
		case "/offline-status":
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		case "/status":
		case "/interpreter":
			queryRequests.Add(1)
			content = `{"elements":[{"type":"node","id":123,"lat":40.1,"lon":-105.1,"tags":{"name":"Water","amenity":"drinking_water"}}]}`
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if _, err := fmt.Fprint(w, content); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	defer server.Close()
	args := []string{
		route, // Cobra accepts flags after the positional path too.
		"--split=2", "--workers=1", "--retries=0", "--fail-fast=false",
		"--name-prefix=Day1-", "--cache-dir=" + filepath.Join(dir, "cache"), "--cache-ttl=1h",
		"--overpass-endpoint=offline=" + server.URL + "/interpreter," + server.URL + "/offline-status",
		"--overpass-endpoint=local=" + server.URL + "/interpreter," + server.URL + "/status,1",
	}
	stdout, diagnostics, err := executeCLI(args...)
	if err != nil {
		t.Fatal(err)
	}
	var fc featureCollection
	if err := json.Unmarshal([]byte(stdout), &fc); err != nil {
		t.Fatalf("stdout is not pure GeoJSON: %v\n%s", err, stdout)
	}
	if fc.Type != "FeatureCollection" || len(fc.Features) != 1 || fc.Features[0].ID != "node/123" || fc.Features[0].Properties.Name != "Day1-Water" {
		t.Fatalf("unexpected GeoJSON: %s", stdout)
	}
	if queryRequests.Load() != 2 {
		t.Fatalf("got %d queries, want 2 splits", queryRequests.Load())
	}
	// Processing and Overpass diagnostics both use Dubplate's JSON logger when
	// redirected, leaving stdout usable by downstream GeoJSON consumers.
	decoder := json.NewDecoder(strings.NewReader(diagnostics))
	var messages []string
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("invalid diagnostic JSON: %v\n%s", err, diagnostics)
		}
		messages = append(messages, fmt.Sprint(record["message"]))
	}
	if !strings.Contains(strings.Join(messages, "\n"), "pois: 1") {
		t.Fatalf("processing diagnostics missing: %s", diagnostics)
	}

	// A zero TTL must still force fresh queries with the new flag parser.
	freshArgs := append(append([]string{}, args...), "--cache-ttl=0")
	if _, _, err := executeCLI(freshArgs...); err != nil {
		t.Fatal(err)
	}
	if queryRequests.Load() != 4 {
		t.Fatalf("zero TTL did not refresh both queries: got %d", queryRequests.Load())
	}
	server.Close()

	for _, output := range []string{"-", filepath.Join(dir, "result.geojson"), ""} {
		t.Run("output="+output, func(t *testing.T) {
			tempOutputDir := t.TempDir()
			t.Setenv("TMPDIR", tempOutputDir)
			if output != "" && output != "-" {
				if err := os.WriteFile(output, bytes.Repeat([]byte("old"), 1000), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cachedArgs := append(append([]string{}, args...), "--out="+output)
			got, _, err := executeCLI(cachedArgs...)
			if err != nil {
				t.Fatalf("offline cache replay failed: %v", err)
			}
			if output != "-" {
				if got != "" {
					t.Fatalf("file output also wrote stdout: %q", got)
				}
				path := output
				if path == "" {
					entries, err := os.ReadDir(tempOutputDir)
					if err != nil || len(entries) != 1 {
						t.Fatalf("temporary output: entries=%v error=%v", entries, err)
					}
					path = filepath.Join(tempOutputDir, entries[0].Name())
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				got = string(data)
			}
			if got != stdout {
				t.Fatalf("output changed:\ngot %s\nwant %s", got, stdout)
			}
		})
	}
}
