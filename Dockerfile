# syntax=docker/dockerfile:1.7

FROM golang:1.24-bookworm AS go-builder
WORKDIR /src
ARG TARGETOS=linux
ARG TARGETARCH=amd64
COPY go.mod ./
COPY cmd ./cmd
COPY coreutils ./coreutils
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags='-s -w' -o /out/groovy-agent ./cmd/agent \
    && CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags='-s -w' -o /out/coreutils-mcp ./cmd/coreutils-mcp

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

FROM llama-runtime AS runtime
# Keep the final image on the pinned llama.cpp runtime base so the copied
# llama-server binaries keep the exact glibc/libstdc++/OpenSSL runtime they were
# built against.
ARG MODEL_FILENAME="Phi-4-mini-instruct.Q8_0.gguf"
ARG MODEL_NAME="Phi-4-mini-instruct"

ENV LLAMA_SERVER_HOST=0.0.0.0 \
    LLAMA_SERVER_PORT=8080 \
    LLAMA_MODEL_FILE=${MODEL_FILENAME} \
    LLAMA_MODEL_NAME=${MODEL_NAME} \
    LLAMA_CTX_SIZE=8192 \
    LLAMA_THREADS=0 \
    LLAMA_N_GPU_LAYERS=0 \
    LLAMA_STARTUP_TIMEOUT=180 \
    LD_LIBRARY_PATH=/opt/llama \
    AGENT_OUTPUT_DIR=/output \
    MCP_HTTP_HOST=0.0.0.0 \
    MCP_HTTP_PORT=8765 \
    MCP_HTTP_PATH=/mcp

COPY --from=go-builder /out/groovy-agent /usr/local/bin/groovy-agent
COPY --from=go-builder /out/coreutils-mcp /usr/local/bin/coreutils-mcp
COPY --from=llama-runtime /app /opt/llama
COPY --from=model-fetch /models/ /models/
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY docker/chat-templates/ /opt/llama/chat-templates/
RUN test -x /opt/llama/llama-server \
    && chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /output

VOLUME /output
EXPOSE 8080 8765
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD []
