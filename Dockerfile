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

FROM model-fetch AS chromium-debian
RUN apt-get update \
    && apt-get install -y --no-install-recommends chromium \
    && mkdir -p /opt/chromium-debian-config/etc/chromium /opt/chromium-debian-config/etc/chromium.d \
    && if [ -d /etc/chromium ]; then cp -a /etc/chromium/. /opt/chromium-debian-config/etc/chromium/; fi \
    && if [ -d /etc/chromium.d ]; then cp -a /etc/chromium.d/. /opt/chromium-debian-config/etc/chromium.d/; fi \
    && mkdir -p /opt/chromium-debian-libs \
    # Exclude the base runtime's core ABI libraries so Chromium uses Ubuntu's
    # libc/libstdc++ family while still picking up Debian-only secondary
    # dependencies from the staged tree below.
    && ldd /usr/lib/chromium/chromium \
      | awk '($3 ~ /^\//) { print $3 }' \
    | grep -Ev '/(ld-linux-x86-64\.so|libc\.so|libdl\.so|libgcc_s\.so|libm\.so|libpthread\.so|libresolv\.so|librt\.so|libstdc\+\+\.so)(\.|$)' \
      | sort -u \
      | xargs -r -I{} sh -c 'mkdir -p "/opt/chromium-debian-libs$(dirname "{}")" && cp -L "{}" "/opt/chromium-debian-libs{}"' \
    && rm -rf /var/lib/apt/lists/*

FROM llama-runtime AS runtime
# Keep the final image on the pinned llama.cpp runtime base so the copied
# llama-server binaries keep the exact glibc/libstdc++/OpenSSL runtime they were
# built against, but copy Debian's real Chromium payload into that runtime so
# `/usr/bin/chromium` is available instead of Ubuntu's Snap launcher wrapper.
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
    WEBUTILS_CHROME_EXECUTABLE=/usr/bin/chromium

COPY --from=go-builder /out/groovy-agent /usr/local/bin/groovy-agent
COPY --from=go-builder /out/coreutils-mcp /usr/local/bin/coreutils-mcp
COPY --from=go-builder /out/webutils-mcp /usr/local/bin/webutils-mcp
COPY --from=llama-runtime /app /opt/llama
COPY --from=model-fetch /models/ /models/
COPY --from=chromium-debian /opt/chromium-debian-config/etc/chromium /etc/chromium
COPY --from=chromium-debian /opt/chromium-debian-config/etc/chromium.d /etc/chromium.d
COPY --from=chromium-debian /usr/bin/chromium /usr/bin/chromium
COPY --from=chromium-debian /usr/lib/chromium /usr/lib/chromium
COPY --from=chromium-debian /usr/share/chromium /usr/share/chromium
COPY --from=chromium-debian /opt/chromium-debian-libs /opt/chromium-debian-libs
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY docker/chat-templates/ /opt/llama/chat-templates/
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        fonts-liberation \
        fonts-noto-color-emoji \
        libgomp1 \
    && rm -rf /var/lib/apt/lists/*
RUN find /opt/chromium-debian-libs -type f -name '*.so*' -printf '%h\n' \
      | sort -u \
      | paste -sd: - \
      > /opt/chromium-debian-lib-path
RUN lib_dirs="$(cat /opt/chromium-debian-lib-path)" \
    && { \
      printf 'if [ -n "$LD_LIBRARY_PATH" ]; then\n'; \
      printf '  export LD_LIBRARY_PATH="$LD_LIBRARY_PATH:/usr/lib/chromium%s"\n' "${lib_dirs:+:$lib_dirs}"; \
      printf 'else\n'; \
      printf '  export LD_LIBRARY_PATH="/usr/lib/chromium%s"\n' "${lib_dirs:+:$lib_dirs}"; \
      printf 'fi\n'; \
    } \
      > /etc/chromium.d/99-groovy-agent-staged-libs
RUN test -x /opt/llama/llama-server \
    && test -x /usr/bin/chromium \
    && chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /output

VOLUME /output
EXPOSE 8080 8765
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD []
