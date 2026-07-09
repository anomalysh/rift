# syntax=docker/dockerfile:1

# --- build ------------------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src

# The Go module lives under projects/server/. Copy the manifests first so `go mod
# download` stays cached until dependencies actually change.
COPY projects/server/go.mod projects/server/go.sum ./
RUN go mod download

COPY projects/server/ ./

# Fully static binary: CGO off means no dynamic loader is needed at runtime,
# which is what keeps the image small and portable across base images.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/riftd ./cmd/riftd

# --- runtime ----------------------------------------------------------------
# Alpine (not distroless) so the image ships busybox wget for the HEALTHCHECK
# below; distroless/static has no HTTP client to probe the health route with.
FROM alpine:3.19 AS runtime

RUN apk add --no-cache ca-certificates \
 && adduser -D -H -u 10001 rift

COPY --from=build /out/riftd /usr/local/bin/riftd

# Run unprivileged.
USER rift

# ingress / gateway / admin. Whether any of these is published to the host is
# decided by compose, not here.
EXPOSE 8080 8081 8082

# Liveness probe on the ingress health route (RouteHealth = /healthz). The port
# matches the default RIFT_INGRESS_ADDR (:8080); keep them in sync if you change
# it. Runs as the non-root user against the container-local listener.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
	CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

# OCI image metadata. The publish workflow (and tools/publish-images.sh) inject
# real values via --build-arg; the defaults keep a plain `docker build` emitting
# a valid, if generic, labelled image. Last layer on purpose: only the labels
# change per commit, so the rest of the image stays cached.
ARG IMAGE_SOURCE="https://github.com/anomalysh/rift"
ARG IMAGE_REVISION=""
ARG IMAGE_VERSION="dev"
ARG IMAGE_CREATED=""
ARG IMAGE_LICENSES="NOASSERTION"
ARG IMAGE_TITLE="riftd"
ARG IMAGE_DESCRIPTION="rift self-hosted tunnel server"
LABEL org.opencontainers.image.source="$IMAGE_SOURCE" \
      org.opencontainers.image.revision="$IMAGE_REVISION" \
      org.opencontainers.image.version="$IMAGE_VERSION" \
      org.opencontainers.image.created="$IMAGE_CREATED" \
      org.opencontainers.image.licenses="$IMAGE_LICENSES" \
      org.opencontainers.image.title="$IMAGE_TITLE" \
      org.opencontainers.image.description="$IMAGE_DESCRIPTION"

ENTRYPOINT ["riftd"]
