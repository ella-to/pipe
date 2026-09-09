# syntax=docker/dockerfile:1

# Builds every pipe example command into one small image. Pick the command when
# you run the container:
#
#   docker build -t pipe-examples -f examples/docker/Dockerfile .
#   docker run --rm -p 8080:8080 -e PIPE_SIGNAL_TOKENS="alice=...,bob=..." \
#       pipe-examples signaling -listen :8080
#   docker run --rm --network host -e PIPE_TURN_SECRET=... \
#       pipe-examples turnserver -listen 0.0.0.0:3478 -relay-ip 203.0.113.10 -relay-ports 49152-49252
#
# The build context must be the repository root, because the examples import
# the module they live in.

FROM golang:1.27-alpine AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/ \
      ./examples/signaling ./examples/turnserver ./examples/turncred ./examples/pipecat ./examples/turnclient

# distroless has no shell and runs as a non-root user; there is nothing in the
# image but the binaries and CA certificates (for turncred/pipecat over HTTPS).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/

# STUN/TURN over UDP and TCP, the default relay range, and the signaling port.
EXPOSE 3478/udp 3478/tcp 49152-49252/udp 8080/tcp

# Health endpoint of the signaling server; harmless for the other commands.
ENTRYPOINT []
CMD ["signaling", "-listen", ":8080"]
