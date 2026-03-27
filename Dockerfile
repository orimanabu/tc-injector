FROM quay.io/centos/centos:stream10-minimal AS builder

USER root
WORKDIR /workspace
COPY go.mod go.sum ./

RUN microdnf install -y go-toolset
RUN go mod download

COPY cmd/       cmd/
COPY pkg/       pkg/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /tc-injector ./cmd/

# Runtime image — must include iproute for the tc(8) binary.
FROM quay.io/centos/centos:stream10-minimal

RUN microdnf install -y iproute iproute-tc && microdnf clean all

COPY --from=builder /tc-injector /tc-injector

ENTRYPOINT ["/tc-injector"]
