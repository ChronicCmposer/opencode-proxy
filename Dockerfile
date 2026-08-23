# syntax=docker/dockerfile:1
#
# Built with `buildctl` against a standalone buildkitd (containerd worker),
# not the Docker CLI — see Makefile and README.md. The frontend is still
# dockerfile.v0, which BuildKit speaks natively regardless of daemon.
#
# Only the --remote half ships as a container: it's what runs on the EC2
# host. --local stays a native binary on the Mac (see launchd/).

FROM --platform=$BUILDPLATFORM golang:1.24 AS build
WORKDIR /src
ARG TARGETOS=linux
ARG TARGETARCH=arm64
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/opencode-proxy ./cmd/opencode-proxy

# Static binary, no libc, nothing else — the image never needs a shell or
# package manager since the proxy has no runtime dependencies of its own.
FROM scratch
COPY --from=build /out/opencode-proxy /opencode-proxy
ENTRYPOINT ["/opencode-proxy"]
