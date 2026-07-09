# syntax=docker/dockerfile:1

# --- build ------------------------------------------------------------------
FROM golang:1.21-alpine AS build
WORKDIR /src

# The Go module lives under server/. Copy the manifests first so `go mod
# download` stays cached until dependencies actually change.
COPY server/go.mod server/go.sum ./
RUN go mod download

COPY server/ ./

# Fully static binary: CGO off means no dynamic loader is needed at runtime,
# which is what keeps the image small and portable across base images.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/tunld ./cmd/tunld

# --- runtime ----------------------------------------------------------------
# Alpine (not distroless) so the image ships busybox wget for the HEALTHCHECK
# below; distroless/static has no HTTP client to probe the health route with.
FROM alpine:3.19 AS runtime

RUN apk add --no-cache ca-certificates \
 && adduser -D -H -u 10001 tunl

COPY --from=build /out/tunld /usr/local/bin/tunld

# Run unprivileged.
USER tunl

# ingress / gateway / admin. Whether any of these is published to the host is
# decided by compose, not here.
EXPOSE 8080 8081 8082

# Liveness probe on the ingress health route (RouteHealth = /healthz). The port
# matches the default TUNL_INGRESS_ADDR (:8080); keep them in sync if you change
# it. Runs as the non-root user against the container-local listener.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
	CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["tunld"]
