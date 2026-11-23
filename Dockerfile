# Pinned by digest: bump deliberately (docker buildx imagetools inspect golang:1.27-alpine). Matches go.mod, so
# nothing downloads a toolchain during the build.
# The compiler always runs on the build machine's own architecture and cross-compiles: building the arm64 image
# under QEMU emulation instead takes the better part of an hour.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
ARG TARGETOS TARGETARCH
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /backshots ./cmd

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
# rsync and sqlite are what the movers shell out to, restic is the optional deduplicating engine.
# No ssh: data only travels over NFS mounts.
RUN apk add --no-cache rsync sqlite ca-certificates restic
# The runner passes --insecure-no-password to every restic command (needs restic >= 0.17), so fail the build
# here rather than every restic backup run at once. Also proves the binary is functional, not just installed.
RUN restic version && restic --insecure-no-password --repo /tmp/restic-probe init && rm -rf /tmp/restic-probe
COPY --from=build /backshots /backshots
# The image runs as root on purpose. Mover pods must preserve file ownership (chown) and read
# arbitrary PVC contents, which needs root. The manager Deployment does not need that: the chart
# runs it as uid 65532 with a read-only root filesystem and no capabilities.
ENTRYPOINT ["/backshots"]
CMD ["manager"]
