# Build go
FROM golang:1.26.8-alpine AS builder
WORKDIR /app
COPY . .
ENV CGO_ENABLED=0
RUN GOEXPERIMENT=jsonv2 go mod download
RUN GOEXPERIMENT=jsonv2 go build -p 2 -trimpath -ldflags="-s -w" -o v2node

# Release
FROM  alpine
# 安装必要的工具包
RUN  apk --update --no-cache add tzdata ca-certificates \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN mkdir /etc/v2node/
COPY --from=builder /app/v2node /usr/local/bin

ENTRYPOINT [ "v2node", "server", "--config", "/etc/v2node/config.json"]
