package main

import (
	"fmt"
	"io"
	stdlog "log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glynternet/pkg/log"
	"github.com/spf13/cobra"
)

func buildCmdTree(logger log.Logger, out io.Writer, rootCmd *cobra.Command) {
	rootCmd.Use = appName + " [flags] <gpx-file>"
	rootCmd.Short = "Find points of interest along a GPX route"
	rootCmd.Args = cobra.ExactArgs(1)
	rootCmd.SetOut(out)
	// Dubplate reports returned errors. Keep diagnostics out of the GeoJSON stream
	// and avoid printing the same error again through Cobra.
	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true

	flags := rootCmd.Flags()
	namePrefix := flags.String("name-prefix", "", "prefix to place in front of all points")
	split := flags.Uint("split", 5, "number of chunks to split the route into for querying overpass API")
	output := flags.String("out", "-", "output file; '-' writes to stdout, an empty value creates a temporary file")
	workers := flags.Int("workers", 0, "number of concurrent workers for API requests (0=auto-detect from API rate limit)")
	retries := flags.Int("retries", 5, "number of retries per API request on transient failures")
	failFast := flags.Bool("fail-fast", true, "stop processing on first API error")

	defaultCacheDir := ".route-poi-finder-state"
	if homeDir, err := os.UserHomeDir(); err == nil {
		defaultCacheDir = filepath.Join(homeDir, defaultCacheDir)
	} else {
		// Failure to write a diagnostic must not prevent using the local fallback.
		_ = log.Warn(logger, log.Message("Unable to determine home directory for default cache directory"), log.ErrorMessage(err))
	}
	cacheDir := flags.String("cache-dir", defaultCacheDir, "directory to cache results in")
	cacheTTL := flags.Duration("cache-ttl", 28*24*time.Hour, "maximum age of cached API responses before re-querying")
	var endpoints endpointFlag
	flags.Var(&endpoints, "overpass-endpoint", "Overpass server as NAME=INTERPRETER_URL,STATUS_URL[,CONCURRENCY] (repeatable). CONCURRENCY only used when the server reports unlimited rate. If unset, defaults to overpass-api.de and overpass.private.coffee.")

	rootCmd.RunE = func(cmd *cobra.Command, args []string) error {
		if *workers < 0 {
			return fmt.Errorf("--workers must be at least 0")
		}
		if *retries < 0 {
			return fmt.Errorf("--retries must be at least 0")
		}
		specs := endpoints.specs
		if !endpoints.set {
			specs = defaultEndpoints()
		}

		// The existing processing code and Overpass client use the standard logger.
		// Route those messages through Dubplate too, restoring it when this run ends.
		writer, flags, prefix := stdlog.Writer(), stdlog.Flags(), stdlog.Prefix()
		stdlog.SetOutput(loggerWriter{logger})
		stdlog.SetFlags(0)
		stdlog.SetPrefix("")
		defer func() {
			stdlog.SetOutput(writer)
			stdlog.SetFlags(flags)
			stdlog.SetPrefix(prefix)
		}()
		return runRoute(cmd.Context(), out, args[0], *namePrefix, *split, *workers, *retries, *failFast, *cacheDir, *cacheTTL, *output, specs)
	}
}

// Type satisfies pflag.Value without splitting the commas inside endpoint specs.
func (*endpointFlag) Type() string { return "endpoint" }

type loggerWriter struct{ logger log.Logger }

func (w loggerWriter) Write(p []byte) (int, error) {
	if err := w.logger.Log(log.Message(strings.TrimSuffix(string(p), "\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}
