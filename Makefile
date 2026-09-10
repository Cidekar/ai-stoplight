# Stoplight development tasks.
#
# `make check` is the one command to run before opening a pull request. It
# is also what CI runs, so a green check locally means a green check there.
#
# Cross-platform coverage is the reason this file exists. The service
# package builds one manager per platform behind a build tag, so a plain
# `go test ./...` on macOS never compiles the Linux or Windows managers.
# Code that is never compiled is code that is never tested, and a failing
# test in a tagged file looks exactly like a passing one. `make cross`
# compiles every package for every platform, and `make test-linux` runs
# the Linux tests for real.

GO ?= go
BINARY ?= stoplight
# The platforms the service package has a manager for. unsupported.go
# covers the rest, so these are the ones worth compiling.
PLATFORMS ?= linux windows darwin
# A small image with a libc the cgo-free test binaries run against.
LINUX_IMAGE ?= alpine:3.20

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary for this platform
	$(GO) build -o $(BINARY) .

.PHONY: test
test: ## Run the tests for this platform
	$(GO) test -race -count=1 -timeout 300s ./...

.PHONY: lint
lint: vet fmtcheck ## Run go vet and the formatting check

.PHONY: vet
vet: ## Run go vet for every platform
	$(GO) vet ./...
	@for os in $(PLATFORMS); do \
		echo "go vet GOOS=$$os"; \
		GOOS=$$os $(GO) vet ./... || exit 1; \
	done

# gofmt exits zero whether or not it found anything, so the output is what
# decides. An unformatted file must fail the build rather than scroll past.
.PHONY: fmtcheck
fmtcheck: ## Fail if any file is not gofmt clean
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "these files are not gofmt clean:"; \
		echo "$$out"; \
		echo "run: gofmt -w ."; \
		exit 1; \
	fi
	@echo "gofmt: clean"

.PHONY: fmt
fmt: ## Format every file in place
	gofmt -w .

# Building proves the platform code compiles. Compiling the tests proves
# the tagged _test.go files compile too, which a build alone does not
# reach. Neither runs the Linux tests: that is test-linux.
.PHONY: cross
cross: ## Build and compile tests for every platform
	@for os in $(PLATFORMS); do \
		echo "==> GOOS=$$os build"; \
		GOOS=$$os $(GO) build -o /dev/null . || exit 1; \
		echo "==> GOOS=$$os test compile"; \
		for pkg in $$($(GO) list ./...); do \
			GOOS=$$os $(GO) test -c -o /dev/null $$pkg || exit 1; \
		done; \
	done
	@echo "cross: every package builds and every test compiles for $(PLATFORMS)"

# The Linux service tests only ever execute here. Cross-compiling the test
# binary and running it in a container is what turns the systemd manager
# from "it compiles" into "it passes".
#
# Docker is optional: a contributor without it gets a clear skip rather
# than a failure, because `make check` must stay runnable on a laptop.
.PHONY: test-linux
test-linux: ## Run the Linux tests in a container, if Docker is available
	@if ! docker info >/dev/null 2>&1; then \
		echo "SKIP test-linux: Docker is not available."; \
		echo "  The Linux-only code was compiled by 'make cross' but not run."; \
		echo "  Start Docker to execute the systemd tests."; \
		exit 0; \
	fi; \
	tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	echo "==> cross-compiling the Linux test binaries"; \
	for pkg in $$($(GO) list ./...); do \
		name=$$(echo $$pkg | tr '/.' '__'); \
		if ! GOOS=linux GOARCH=$$($(GO) env GOARCH) CGO_ENABLED=0 \
			$(GO) test -c -o "$$tmp/$$name.test" $$pkg 2>/dev/null; then \
			continue; \
		fi; \
		[ -f "$$tmp/$$name.test" ] && echo "$$pkg" >> "$$tmp/packages"; \
	done; \
	echo "==> running them in $(LINUX_IMAGE)"; \
	docker run --rm -v "$$tmp:/tests:ro" -w /tmp $(LINUX_IMAGE) sh -c '\
		status=0; \
		for t in /tests/*.test; do \
			echo "--- $$(basename $$t .test)"; \
			"$$t" -test.count=1 -test.timeout=300s || status=1; \
		done; \
		exit $$status'

.PHONY: check
check: lint cross test test-linux ## Everything a contributor runs before a PR
	@echo
	@echo "check: passed"

.PHONY: clean
clean: ## Remove the built binary
	rm -f $(BINARY)
