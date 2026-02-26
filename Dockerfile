FROM golang:1.25 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION:-dev}" -o /sagadata-csi ./cmd/sagadata-csi

# Node plugin needs filesystem tools (mkfs.ext4, mkfs.xfs, blkid, mount).
# Use Debian slim instead of distroless.
FROM debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        e2fsprogs \
        xfsprogs \
        util-linux \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=builder /sagadata-csi /sagadata-csi
ENTRYPOINT ["/sagadata-csi"]
