# syntax=docker/dockerfile:1.7
# Go 1.27 builds the HTTP/MCP/configuration control plane.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG BUILD_SHA
ARG BUILD_TAG

WORKDIR /src
COPY go.mod ./
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

# The legacy task engine remains a compatibility worker for existing Python
# regexes, plugins, and notification providers while the service boundary is Go.
FROM python:3.13-alpine AS runtime

RUN apk add --no-cache su-exec

WORKDIR /app
ENV TZ=Asia/Shanghai \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt

COPY --from=builder /out/quark-auto-save /usr/local/bin/quark-auto-save
COPY app/static /app/app/static
COPY app/templates /app/app/templates
COPY app/sdk /app/app/sdk
COPY app/_clean_plugins.py /app/app/_clean_plugins.py
COPY app/runtime /app/app/runtime
COPY app/runtime/run.py /app/app/run.py
COPY plugins /app/plugins
COPY quark_auto_save.py notify.py quark_config.json /app/
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN python3 /app/app/_clean_plugins.py && rm -f /app/app/_clean_plugins.py \
    && addgroup -S -g 1001 qas \
    && adduser -S -D -u 1000 -G qas qas \
    && mkdir -p /app/config /media \
    && chown -R qas:qas /app /media \
    && chmod 0755 /usr/local/bin/docker-entrypoint.sh

EXPOSE 5005
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:5005/login', timeout=3)" || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
