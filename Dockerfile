# Build go
FROM golang:1.26.1-alpine AS builder
WORKDIR /app
COPY . .
ENV CGO_ENABLED=0
RUN GOEXPERIMENT=jsonv2 go mod download
RUN GOEXPERIMENT=jsonv2 go build -ldflags="-s -w" -trimpath -v -o v2node .

# Release
FROM alpine:latest
RUN apk --update --no-cache add tzdata ca-certificates \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN mkdir -p /etc/v2node/
COPY --from=builder /app/v2node /usr/local/bin/

ENV GOGC=80
ENV GOMEMLIMIT=512MiB

ENTRYPOINT [ "v2node", "server", "--config", "/etc/v2node/config.json"]
