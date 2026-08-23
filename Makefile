# Explicit build entry points for opencode-proxy.
#
# Image builds go through `buildctl` against a standalone buildkitd running
# the containerd worker — no Docker CLI or daemon involved. This assumes
# buildkitd is already running and reachable at $BUILDKIT_HOST (defaults to
# buildkitd's standard rootless socket); see README.md "Setting up
# buildkitd" for the one-time daemon setup.

BINARY        := opencode-proxy
IMAGE_TAR     := opencode-proxy.tar
IMAGE_NAME    := opencode-proxy
IMAGE_PLATFORM := linux/arm64
BUILDKIT_HOST ?= unix:///run/buildkit/buildkitd.sock

.PHONY: all build test fmt vet lint image clean check-buildkitd

all: build

## Native build of the CLI binary (host OS/ARCH) for local dev/testing.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/opencode-proxy

test:
	go test ./...

fmt:
	gofmt -l .

vet:
	go vet ./...

lint: fmt vet

## Verify buildkitd is reachable before attempting an image build; buildctl's
## own error on a dead socket is unhelpfully generic otherwise.
check-buildkitd:
	@BUILDKIT_HOST=$(BUILDKIT_HOST) buildctl debug workers >/dev/null || \
	  (echo "error: buildkitd not reachable at $(BUILDKIT_HOST) — see README.md 'Setting up buildkitd'" >&2; exit 1)

## Builds the linux/arm64 OCI image (the same image runs both --local and
## --remote) and writes it out as an OCI archive tar, plus a .sha256
## checksum file — both are published as GitHub Release assets. The
## checksum file is what opencode-proxy-update.timer polls every 6h on each
## host to detect a new version without re-downloading the full image.
image: check-buildkitd
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
