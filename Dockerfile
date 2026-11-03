# syntax=docker/dockerfile:1

# Build stage. It always runs on the build machine's own architecture and
# cross-compiles, so multi-arch images need no emulation (smokealarm is pure Go).
ARG GO_VERSION=1.27.3
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

# Dependencies first: this layer is reused as long as go.mod and go.sum stay the same.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/smokealarm ./cmd/smokealarm
# The distroless image has no shell, so the data directory is prepared here.
RUN mkdir /out-data

# Final image: no shell, no package manager, runs as a non-root user.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/smokealarm /smokealarm
COPY --from=build --chown=65532:65532 /out-data /data

# The database lives in /data (a volume in Kubernetes). SQLite temporary files
# go there too, so that /data is the only place the process writes to.
ENV SMOKEALARM_DB_PATH=/data/smokealarm.db \
    TMPDIR=/data

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/smokealarm"]
