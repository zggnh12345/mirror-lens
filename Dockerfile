# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/mirror-lens .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 lens
COPY --from=build /out/mirror-lens /usr/local/bin/mirror-lens
ENV DATA_DIR=/data TZ=Asia/Shanghai
RUN mkdir -p /data && chown lens:lens /data
USER lens
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["mirror-lens"]
