.PHONY: fmt vet lint build install dist test test-integration check

# Release builds must be told their version: plain `go build` cannot read the
# tag it was built from. `git describe` marks a dirty tree so a local build is
# never mistaken for a release.
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo devel)
LDFLAGS := -X github.com/hyj28/unring/internal/cli.Version=$(VERSION)

fmt:
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
		echo "The following files need gofmt:"; \
		echo "$$files"; \
		exit 1; \
	fi

vet:
	go vet ./...

lint: fmt vet

build:
	go build ./...

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/unring

# Release binaries with their checksums. pg_query_go is a cgo dependency, so a
# target needs its own C toolchain: this builds only for the host platform, and
# other platforms have to be built on a machine of that platform.
dist:
	@rm -rf dist && mkdir -p dist
	@os=$$(go env GOOS); arch=$$(go env GOARCH); \
		echo "building unring $(VERSION) for $$os/$$arch"; \
		go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/unring-$(VERSION)-$$os-$$arch ./cmd/unring
	@cd dist && shasum -a 256 unring-* > SHA256SUMS && cat SHA256SUMS

test:
	go test ./...

test-integration:
	UNRING_REQUIRE_POSTGRES=1 go test -count=1 ./...

check: fmt vet build test
