FROM golang:1.25 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION:-dev}" -o /epilayer-csi ./cmd/epilayer-csi

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
COPY --from=builder /epilayer-csi /epilayer-csi
ENTRYPOINT ["/epilayer-csi"]
