# Build stage
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

# Set the working directory
WORKDIR /app

# Copy go.mod and go.sum files
COPY go.mod go.sum ./

# Download dependencies
# Modules through nora (packages.maestra.io, 3-day hold, issues-maestra#1649): CI passes
# its token as the PRIVATE_PACKAGES_TOKEN secret; a local build keeps the default proxy.
RUN --mount=type=secret,id=PRIVATE_PACKAGES_TOKEN,env=NORA_TOKEN \
    GOPROXY="${NORA_TOKEN:+https://token:${NORA_TOKEN}@packages.maestra.io/go}" go mod download

# Copy the source code
COPY . .

# Build the application using cross-compilation instead of QEMU emulation.
# TARGETOS and TARGETARCH are automatically set by Docker BuildKit.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o mcp-grafana ./cmd/mcp-grafana

# Final stage
FROM debian:bookworm-slim@sha256:67b30a61dc87758f0caf819646104f29ecbda97d920aaf5edc834128ac8493d3

LABEL io.modelcontextprotocol.server.name="io.github.grafana/mcp-grafana"

# Install ca-certificates for HTTPS requests and upgrade existing packages
# to pick up security fixes (e.g. OpenSSL) newer than the base image snapshot
# BASE_REFRESH (CI passes the ISO week): the layer cache would otherwise serve
# this stage forever, without apt/OS security updates. A new value rebuilds it
# on the next build.
ARG BASE_REFRESH
RUN --mount=type=secret,id=PRIVATE_PACKAGES_TOKEN,required=true \
    --mount=type=bind,from=public.ecr.aws/g5f9s8a4/nora-apt:1.0.0@sha256:cd5fcec38f8b7c4fdd283d41f49ac656751d4deed168111f89f44976a43a8178,target=/nora-apt,rw \
    . /nora-apt/env.sh && apt-get update && apt-get upgrade -y && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*

# Create a non-root user
RUN useradd -r -u 1000 -m mcp-grafana

# Set the working directory
WORKDIR /app

# Copy the binary from the builder stage
COPY --from=builder --chown=1000:1000 /app/mcp-grafana /app/

# Use the non-root user
USER mcp-grafana

# Expose the port the app runs on
EXPOSE 8000

# Run the application
ENTRYPOINT ["/app/mcp-grafana", "--transport", "sse", "--address", "0.0.0.0:8000"]
