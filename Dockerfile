# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# RBR-MP-Server - build stage
#
# The server is pure Go with no cgo and no runtime dependencies, so the final
# image is a static binary on top of scratch: a few megabytes, no shell, no
# package manager, nothing to patch.
# ---------------------------------------------------------------------------
FROM golang:1.22-alpine AS build

WORKDIR /src

# Dependencies first, so the module download layer caches across code changes.
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/rbrmp-server ./cmd/rbrmp-server

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
FROM scratch

COPY --from=build /out/rbrmp-server /rbrmp-server

# The one UDP port everything happens on.
EXPOSE 40100/udp

# Run as a non-root uid (scratch has no /etc/passwd; a numeric uid is enough).
USER 65534

ENTRYPOINT ["/rbrmp-server"]
# Overridable defaults: docker run <image> -tick 60 -echo 0 ...
CMD ["-addr", ":40100"]
