# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG BUILD_SHA
ARG BUILD_TAG

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal

RUN set -eux; \
    if [ "${TARGETARCH}" = "arm" ]; then \
      CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH=arm GOARM="${TARGETVARIANT#v}" \
        go build -trimpath -ldflags="-s -w -X main.BuildSHA=${BUILD_SHA} -X main.BuildTag=${BUILD_TAG}" \
        -o /out/quark-auto-save ./cmd/quark-auto-save; \
    else \
      CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
        go build -trimpath -ldflags="-s -w -X main.BuildSHA=${BUILD_SHA} -X main.BuildTag=${BUILD_TAG}" \
        -o /out/quark-auto-save ./cmd/quark-auto-save; \
    fi

FROM alpine:3.22 AS runtime

RUN apk add --no-cache su-exec ca-certificates tzdata

WORKDIR /app
ENV TZ=Asia/Shanghai

COPY --from=builder /out/quark-auto-save /usr/local/bin/quark-auto-save
COPY app/static /app/app/static
COPY app/templates /app/app/templates
COPY quark_config.json /app/quark_config.json
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN addgroup -S -g 1001 qas \
    && adduser -S -D -u 1000 -G qas qas \
    && mkdir -p /app/config /media \
    && chown -R qas:qas /app /media \
    && chmod 0755 /usr/local/bin/docker-entrypoint.sh

EXPOSE 5005
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:5005/login || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
