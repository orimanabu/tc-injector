FROM golang:1.22-alpine AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/       cmd/
COPY pkg/       pkg/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /tc-injector ./cmd/

# Runtime image — must include iproute2 for the tc(8) binary.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        iproute2 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /tc-injector /tc-injector

ENTRYPOINT ["/tc-injector"]
