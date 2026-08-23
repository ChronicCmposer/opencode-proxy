# syntax=docker/dockerfile:1
#
# Built with `buildctl` against a standalone buildkitd, not the Docker CLI
# — see Makefile. This same image runs both --remote (EC2) and --local
# (vm/deploy-local.sh), both under containerd/ctr.

FROM --platform=$BUILDPLATFORM golang:1.24.7 AS build
WORKDIR /src
ARG TARGETOS=linux
ARG TARGETARCH=arm64
COPY go.mod go.sum ./
RUN go mod download
# version.go is generated on the host (make image depends on
# generate-version) and picked up here along with the rest of the *.go
# files — regenerating it in-container would need .git history in the
# build context.
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/opencode-proxy .

FROM scratch
COPY --from=build /out/opencode-proxy /opencode-proxy
ENTRYPOINT ["/opencode-proxy"]
