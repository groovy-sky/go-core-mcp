# syntax=docker/dockerfile:1.7

FROM golang:1.24-bookworm AS go-builder
WORKDIR /src
ARG TARGETOS=linux
ARG TARGETARCH=amd64
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY coreutils ./coreutils
COPY internal ./internal
COPY webutils ./webutils
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags='-s -w' -o /out/groovy-agent ./cmd/agent \
    && CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags='-s -w' -o /out/coreutils-mcp ./cmd/coreutils-mcp \
    && CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags='-s -w' -o /out/webutils-mcp ./cmd/webutils-mcp

FROM ghcr.io/ggml-org/llama.cpp:server@sha256:092d1291f2bcf59ff727fa3af855fb9bd4759d6bff860f6fbfd5e3e377e12625 AS llama-runtime

FROM debian:bookworm-slim AS model-fetch
ARG DOWNLOAD_MODEL=0
ARG MODEL_URL="https://huggingface.co/unsloth/Phi-4-mini-instruct-GGUF/resolve/main/Phi-4-mini-instruct.Q8_0.gguf"
ARG MODEL_FILENAME="Phi-4-mini-instruct.Q8_0.gguf"
ARG MODEL_NAME="Phi-4-mini-instruct"
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /models
RUN --mount=type=secret,id=hf_token \
    set -eu; \
    if [ "$DOWNLOAD_MODEL" = "1" ]; then \
      token=""; \
      if [ -f /run/secrets/hf_token ]; then token="$(cat /run/secrets/hf_token)"; fi; \
      if [ -n "$token" ]; then \
        curl -fL --retry 5 --retry-delay 2 --retry-all-errors --oauth2-bearer "$token" "$MODEL_URL" -o "/models/$MODEL_FILENAME"; \
      else \
        curl -fL --retry 5 --retry-delay 2 --retry-all-errors "$MODEL_URL" -o "/models/$MODEL_FILENAME"; \
      fi; \
    fi

FROM debian:bookworm-slim AS chromium-debian
RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends chromium; \
    mkdir -p /chromium-rootfs /chromium-rootfs/opt/chromium-libs; \
    copy_package_files() { \
      pkg="$1"; \
      dest="$2"; \
      dpkg-query -L "$pkg" | while read -r path; do \
        [ -n "$path" ] || continue; \
        [ -e "$path" ] || continue; \
        case "$path" in \
          /usr/share/doc/*|/usr/share/lintian/*|/usr/share/man/*|/usr/share/menu/*|/usr/share/bug/*) \
            continue ;; \
        esac; \
        if [ -d "$path" ]; then \
          mkdir -p "$dest$path"; \
        else \
          mkdir -p "$dest$(dirname "$path")"; \
          cp -a "$path" "$dest$path"; \
        fi; \
      done; \
    }; \
    payload_packages='chromium chromium-common'; \
    dependency_packages="$( \
      apt-cache depends --recurse --no-recommends --no-suggests --no-conflicts --no-breaks --no-replaces --no-enhances chromium chromium-common \
        | sed -n 's/^[| ]*PreDepends: //p; s/^[| ]*Depends: //p' \
        | tr -d '<>' \
        | awk '$1 != "" && $1 != "chromium" && $1 != "chromium-common" && $1 != "libc6" { print $1 }' \
        | sort -u \
    )"; \
    for pkg in $payload_packages; do \
      copy_package_files "$pkg" /chromium-rootfs; \
    done; \
    for pkg in $dependency_packages; do \
      copy_package_files "$pkg" /chromium-rootfs/opt/chromium-libs; \
    done; \
    rm -rf /var/lib/apt/lists/*

FROM llama-runtime AS runtime
# Keep the final image on the pinned llama.cpp runtime base so the copied
# llama-server binaries keep the exact glibc/libstdc++/OpenSSL runtime they were
# built against. Bundle Debian's Chromium payload plus its runtime dependency
# closure, then launch it through a small wrapper instead of mixing an Ubuntu
# Snap stub with copied browser fragments or a second package install strategy.
ARG MODEL_FILENAME="Phi-4-mini-instruct.Q8_0.gguf"
ARG MODEL_NAME="Phi-4-mini-instruct"

ENV LLAMA_SERVER_HOST=0.0.0.0 \
    LLAMA_SERVER_PORT=8080 \
    LLAMA_MODEL_FILE=${MODEL_FILENAME} \
    LLAMA_MODEL_NAME=${MODEL_NAME} \
    LLAMA_CTX_SIZE=81920 \
    LLAMA_THREADS=0 \
    LLAMA_N_GPU_LAYERS=0 \
    LLAMA_STARTUP_TIMEOUT=180 \
    LD_LIBRARY_PATH=/opt/llama \
    AGENT_OUTPUT_DIR=/output \
    MCP_HTTP_HOST=0.0.0.0 \
    MCP_HTTP_PORT=8765 \
    MCP_HTTP_PATH=/mcp \
    LLAMA_MCP_WEBUTILS=0 \
    WEBUTILS_CHROME_EXECUTABLE=/usr/bin/chromium \
    WEBUTILS_CHROME_ARGS="--no-sandbox --disable-dev-shm-usage"

COPY --from=go-builder /out/groovy-agent /usr/local/bin/groovy-agent
COPY --from=go-builder /out/coreutils-mcp /usr/local/bin/coreutils-mcp
COPY --from=go-builder /out/webutils-mcp /usr/local/bin/webutils-mcp
COPY --from=llama-runtime /app /opt/llama
COPY --from=model-fetch /models/ /models/
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY docker/chat-templates/ /opt/llama/chat-templates/
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        fonts-liberation \
        fonts-noto-color-emoji \
        libgomp1 \
        libnspr4 \
        libnss3 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=chromium-debian /chromium-rootfs/ /
COPY docker/chromium-wrapper.sh /usr/bin/chromium
RUN test -x /opt/llama/llama-server \
    && chmod +x /usr/bin/chromium \
    && test -x /usr/bin/chromium \
    && /usr/bin/chromium --version >/dev/null \
    && chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /output

VOLUME /output
EXPOSE 8080 8765
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD []
