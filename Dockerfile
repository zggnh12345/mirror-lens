# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux
# 版本由 CI 校验后传入（scripts/check-version.sh）；本地构建可省略。
# VERSION 只用于镜像标签，程序版本仍以 internal/version.Version 为准。
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 非 dev 构建必须在镜像内自证：build-arg 的 VERSION 与 Go 常量一致。
RUN if [ "$VERSION" != "dev" ]; then \
      want=$(sed -n 's/^const Version = "\([^"]*\)"$/\1/p' internal/version/version.go); \
      [ "$VERSION" = "$want" ] || { echo "VERSION($VERSION) != Go Version($want)" >&2; exit 1; }; \
    fi
RUN go build -trimpath \
      -ldflags="-s -w -X github.com/zggnh12345/mirror-lens/internal/version.Commit=${COMMIT} -X github.com/zggnh12345/mirror-lens/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/mirror-lens .

FROM alpine:3.22
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="mirror-lens" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 lens
COPY --from=build /out/mirror-lens /usr/local/bin/mirror-lens
ENV DATA_DIR=/data TZ=Asia/Shanghai
RUN mkdir -p /data && chown lens:lens /data
USER lens
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["mirror-lens"]
