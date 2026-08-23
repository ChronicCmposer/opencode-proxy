# `image` assumes a standalone buildkitd is already running and reachable
# at $BUILDKIT_HOST — see README.md "Setting up buildkitd".

BINARY        := opencode-proxy
IMAGE_TAR     := opencode-proxy.tar
IMAGE_NAME    := opencode-proxy
IMAGE_PLATFORM := linux/arm64
BUILDKIT_HOST ?= unix:///run/buildkit/buildkitd.sock

.PHONY: all build test fmt vet lint image clean check-buildkitd \
        generate-version bump-version release

all: build

generate-version:
	scripts/generate-version.sh

build: generate-version
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) .

test: generate-version
	go test ./...

fmt:
	gofmt -l .

vet: generate-version
	go vet ./...

lint: fmt vet

bump-version:
	@if [ -z "$(LEVEL)" ]; then \
	  echo "usage: make bump-version LEVEL=major|minor|patch" >&2; exit 1; \
	fi
	scripts/bump-version.sh "$(LEVEL)"

release:
	scripts/release.sh

# buildctl's own error on a dead socket is unhelpfully generic.
check-buildkitd:
	@BUILDKIT_HOST=$(BUILDKIT_HOST) buildctl debug workers >/dev/null || \
	  (echo "error: buildkitd not reachable at $(BUILDKIT_HOST) — see README.md 'Setting up buildkitd'" >&2; exit 1)

image: check-buildkitd generate-version
	BUILDKIT_HOST=$(BUILDKIT_HOST) buildctl build \
	  --frontend dockerfile.v0 \
	  --local context=. \
	  --local dockerfile=. \
	  --opt platform=$(IMAGE_PLATFORM) \
	  --opt build-arg:TARGETARCH=arm64 \
	  --output type=oci,name=docker.io/library/$(IMAGE_NAME):latest,dest=$(IMAGE_TAR)
	sha256sum $(IMAGE_TAR) > $(IMAGE_TAR).sha256
	@echo "wrote $(IMAGE_TAR) and $(IMAGE_TAR).sha256 — attach both to the GitHub release"

clean:
	rm -f $(BINARY) $(IMAGE_TAR) $(IMAGE_TAR).sha256
