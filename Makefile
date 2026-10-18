COMPONENTS ?= route-poi-finder

include ./dubplate.Makefile
include ./go.Makefile

.PHONY: test check-format check
test:
	go test -race ./...

check-format:
	@test -z "$$(gofmt -l cmd overpass)" || { gofmt -l cmd overpass; exit 1; }

check: check-format test binaries
	$(MAKE) test-binary-version-output APP_NAME=route-poi-finder
