# syntax=docker/dockerfile:1
FROM golang:1.26.1-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/xray-core/go.mod third_party/xray-core/go.sum ./third_party/xray-core/
ENV CGO_ENABLED=0 GOEXPERIMENT=jsonv2 GOTOOLCHAIN=local
RUN go mod download && go mod verify

COPY . .
ARG VERSION=v0.4.4-ram3
RUN go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X github.com/wyx2685/v2node/cmd.version=${VERSION}" \
    -o /out/v2node ./

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /etc/v2node \
    && chmod 0750 /etc/v2node
COPY --from=builder /out/v2node /usr/local/bin/v2node

STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/v2node"]
CMD ["server", "--config", "/etc/v2node/config.json"]
