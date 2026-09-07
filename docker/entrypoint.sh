#!/usr/bin/env bash
set -euo pipefail

# Save the container's original stdin on fd 3 before any subshell or
# redirection may replace fd 0 (e.g. the curl readiness loop), so the agent is
# started with the stdin the container was launched with.  The agent itself is
# a one-shot CLI (it takes the prompt from argv, not stdin); this only keeps
# stdin intact for the tools it spawns and for interactive `docker run -i`
# usage.
exec 3<&0

# Ensure the output directory exists so result JSON files can always be written.
mkdir -p "${AGENT_OUTPUT_DIR:-/output}"

# Waits for a backgrounded process to exit and sets $wait_status to its exit
# code, retrying `wait` after a trapped signal forwarded to that process
# interrupts `wait` early (status > 128) but before the process has actually
# exited. Used by every mode below that simply supervises one long-running
# child process (llama-server in serve-only mode; coreutils-mcp in mcp mode).
wait_for_exit_status() {
  local pid="$1"
  set +e
  wait "$pid"
  wait_status=$?
  while (( wait_status > 128 )) && kill -0 "$pid" 2>/dev/null; do
    wait "$pid"
    wait_status=$?
  done
  set -e
}

# `mcp` is an explicit subcommand (the container's first positional argument),
# distinct from both the default no-prompt llama-server API mode and the
# prompted one-shot `groovy-agent` mode. It serves the bundled read-only
# coreutils MCP tool set over the MCP Streamable HTTP transport so a remote
# MCP-compatible client can connect to it directly. llama-server is not
# started in this mode: the remote MCP service is independent of the
# llama.cpp OpenAI-compatible API and can be published on its own.
if [[ "${1:-}" == "mcp" ]]; then
  shift

  MCP_HTTP_HOST="${MCP_HTTP_HOST:-0.0.0.0}"
  MCP_HTTP_PORT="${MCP_HTTP_PORT:-8765}"
  MCP_HTTP_PATH="${MCP_HTTP_PATH:-/mcp}"
  MCP_HTTP_TOKEN="${MCP_HTTP_TOKEN:-}"
  MCP_WORKSPACE="${MCP_WORKSPACE:-${AGENT_OUTPUT_DIR:-/output}}"
  mkdir -p "$MCP_WORKSPACE"

  mcp_args=(
    --workspace "$MCP_WORKSPACE"
    --transport http
    --listen "${MCP_HTTP_HOST}:${MCP_HTTP_PORT}"
    --http-path "$MCP_HTTP_PATH"
  )
  if [[ -n "$MCP_HTTP_TOKEN" ]]; then
    mcp_args+=(--http-token "$MCP_HTTP_TOKEN")
  fi
  mcp_args+=("$@")

  echo "remote MCP server (Streamable HTTP): http://${MCP_HTTP_HOST}:${MCP_HTTP_PORT}${MCP_HTTP_PATH}" >&2
  echo "workspace: ${MCP_WORKSPACE}" >&2
  echo "publish it with 'docker run -p ${MCP_HTTP_PORT}:${MCP_HTTP_PORT} ...'" >&2
  if [[ -z "$MCP_HTTP_TOKEN" ]]; then
    echo "WARNING: MCP_HTTP_TOKEN is not set. Anyone who can reach the published" >&2
    echo "         port gets unauthenticated access to the read-only filesystem" >&2
    echo "         tools. Only publish this port on a trusted network, or set" >&2
    echo "         MCP_HTTP_TOKEN and require clients to send an" >&2
    echo "         'Authorization: Bearer' header carrying that token." >&2
  fi

  /usr/local/bin/coreutils-mcp "${mcp_args[@]}" &
  mcp_pid=$!
  trap 'kill -TERM "$mcp_pid" 2>/dev/null || true' TERM
  trap 'kill -INT "$mcp_pid" 2>/dev/null || true' INT

  wait_for_exit_status "$mcp_pid"
  exit "$wait_status"
fi

LLAMA_SERVER_HOST="${LLAMA_SERVER_HOST:-127.0.0.1}"
LLAMA_SERVER_PORT="${LLAMA_SERVER_PORT:-8080}"
LLAMA_MODEL_FILE="${LLAMA_MODEL_FILE:-Phi-4-mini-instruct.Q8_0.gguf}"
LLAMA_MODEL_PATH="${LLAMA_MODEL_PATH:-/models/${LLAMA_MODEL_FILE}}"
LLAMA_MODEL_NAME="${LLAMA_MODEL_NAME:-Phi-4-mini-instruct}"
LLAMA_CTX_SIZE="${LLAMA_CTX_SIZE:-8192}"
LLAMA_THREADS="${LLAMA_THREADS:-0}"
LLAMA_N_GPU_LAYERS="${LLAMA_N_GPU_LAYERS:-0}"
LLAMA_STARTUP_TIMEOUT="${LLAMA_STARTUP_TIMEOUT:-180}"
LLAMA_EXTRA_ARGS="${LLAMA_EXTRA_ARGS:-}"

# Sampling/generation guardrails. These defaults exist to prevent small
# quantized models from spiraling into degenerate token repetition (e.g. an
# assistant turn that repeats the same "Final Answer" paragraph until the
# tool/time budget is exhausted, without ever issuing the required tool
# call). They are applied unconditionally as llama-server startup flags, so
# they take effect even for requests that do not set sampling params
# themselves, and can still be overridden per-request by the client or
# widened/replaced via LLAMA_EXTRA_ARGS below.
#
# - LLAMA_REPEAT_PENALTY: penalize sampling tokens that already appeared
#   recently; the primary defense against repetition loops.
# - LLAMA_REPEAT_LAST_N: how many recent tokens the repeat penalty considers.
# - LLAMA_PREDICT_LIMIT: hard cap (in tokens) on a single generation, so a
#   degenerate loop is cut off quickly instead of running for the entire
#   request timeout. -1 (llama.cpp's "unbounded" default) is accepted to
#   opt back out.
LLAMA_REPEAT_PENALTY="${LLAMA_REPEAT_PENALTY:-1.3}"
LLAMA_REPEAT_LAST_N="${LLAMA_REPEAT_LAST_N:-256}"
LLAMA_PREDICT_LIMIT="${LLAMA_PREDICT_LIMIT:-1024}"

# The bundled llama.cpp build can act as an MCP client itself: it spawns the
# MCP servers listed in --mcp-servers-json (Cursor-compatible format) over
# stdio, discovers their tools at startup and exposes them to the chat flow
# alongside its own tool set. Registering the read-only coreutils server here
# is what makes its tools discoverable from llama.cpp (including its built-in
# Web UI) without any external MCP client.
#
# - LLAMA_MCP_COREUTILS: set to 0 to start llama-server without the bundled
#   MCP tool set.
# - LLAMA_MCP_WORKSPACE: directory the registered tools are confined to; every
#   tool call stays inside it, exactly as in the standalone `mcp` mode.
LLAMA_MCP_COREUTILS="${LLAMA_MCP_COREUTILS:-1}"
LLAMA_MCP_WORKSPACE="${LLAMA_MCP_WORKSPACE:-${MCP_WORKSPACE:-${AGENT_OUTPUT_DIR:-/output}}}"

# Escapes a string for embedding in a JSON string literal. Only backslashes
# and double quotes need escaping for the filesystem paths used below;
# control characters are rejected by the caller instead.
json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '%s' "$value"
}

# `groovy-agent` is a one-shot CLI: it requires a positional prompt and exits
# with a usage error without one.  Mirror the Go flag package's parsing rules
# closely enough to tell whether the container command contains a positional
# prompt, so a no-argument `docker run` can serve the llama-server API instead
# of failing with that usage error.
#
# Every agent flag takes a value, so `-flag value` consumes the next argument
# unless the value is inlined as `-flag=value`.  `--` terminates flag parsing.
# The flag list below must be kept in sync with the flags registered in
# cmd/agent/main.go: an unlisted value-taking flag would make its value look
# like a positional prompt.

# The agent joins its positional arguments and trims them, so whitespace-only
# arguments are not a usable prompt either.
positional_is_prompt() {
  local joined="$*"
  [[ -n "${joined//[[:space:]]/}" ]]
}

has_positional_prompt() {
  while (( $# > 0 )); do
    case "$1" in
      --)
        shift
        positional_is_prompt "$@"
        return $?
        ;;
      -*=*)
        shift
        ;;
      -llama-url|--llama-url|-model|--model|-mcp-command|--mcp-command|-workspace|--workspace)
        shift
        if (( $# > 0 )); then
          shift
        fi
        ;;
      -*)
        # Unrecognized flag: deliberately select one-shot mode (rather than
        # reporting a prompt) so the agent parses the flag and reports the
        # error itself.
        return 0
        ;;
      *)
        positional_is_prompt "$@"
        return $?
        ;;
    esac
  done
  return 1
}

if has_positional_prompt "$@"; then
  agent_mode="oneshot"
else
  agent_mode="serve"
fi

if [[ ! -f "$LLAMA_MODEL_PATH" ]]; then
  echo "model file not found: $LLAMA_MODEL_PATH" >&2
  echo "set LLAMA_MODEL_PATH or mount /models with ${LLAMA_MODEL_FILE}" >&2
  exit 1
fi

if [[ "$LLAMA_THREADS" == "0" ]]; then
  LLAMA_THREADS="$(nproc)"
fi

llama_args=(
  --jinja
  --host "$LLAMA_SERVER_HOST"
  --port "$LLAMA_SERVER_PORT"
  --model "$LLAMA_MODEL_PATH"
  --alias "$LLAMA_MODEL_NAME"
  --ctx-size "$LLAMA_CTX_SIZE"
  --threads "$LLAMA_THREADS"
  --n-gpu-layers "$LLAMA_N_GPU_LAYERS"
  --repeat-penalty "$LLAMA_REPEAT_PENALTY"
  --repeat-last-n "$LLAMA_REPEAT_LAST_N"
  --n-predict "$LLAMA_PREDICT_LIMIT"
)

if [[ "$LLAMA_MCP_COREUTILS" != "0" ]]; then
  if [[ "$LLAMA_MCP_WORKSPACE" != /* ]]; then
    echo "LLAMA_MCP_WORKSPACE must be an absolute path: $LLAMA_MCP_WORKSPACE" >&2
    echo "llama-server spawns the MCP server itself, so a relative path would" >&2
    echo "be resolved against llama-server's working directory." >&2
    exit 1
  fi
  if [[ "$LLAMA_MCP_WORKSPACE" == *[[:cntrl:]]* ]]; then
    echo "LLAMA_MCP_WORKSPACE must not contain control characters" >&2
    exit 1
  fi
  mkdir -p "$LLAMA_MCP_WORKSPACE"
  mcp_servers_json="$(printf '{"mcpServers":{"coreutils":{"command":"/usr/local/bin/coreutils-mcp","args":["--workspace","%s","--transport","stdio"]}}}' \
    "$(json_escape "$LLAMA_MCP_WORKSPACE")")"
  llama_args+=(--mcp-servers-json "$mcp_servers_json")
  echo "registering bundled coreutils MCP server with llama-server (stdio)" >&2
  echo "MCP tool workspace: ${LLAMA_MCP_WORKSPACE} (read-only tools)" >&2
  echo "llama-server limits CORS origins to localhost while MCP servers are" >&2
  echo "enabled; set LLAMA_MCP_COREUTILS=0 to start without the tool set." >&2
fi

if [[ -n "$LLAMA_EXTRA_ARGS" ]]; then
  read -r -a extra_args <<< "$LLAMA_EXTRA_ARGS"
  llama_args+=("${extra_args[@]}")
fi

/opt/llama/llama-server "${llama_args[@]}" &
llama_pid=$!
agent_pid=""

shutdown() {
  kill -TERM "$llama_pid" 2>/dev/null || true
  if [[ -n "$agent_pid" ]]; then
    kill -TERM "$agent_pid" 2>/dev/null || true
  fi
}
trap shutdown INT TERM

deadline=$((SECONDS + LLAMA_STARTUP_TIMEOUT))
while true; do
  if curl -fsS "http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}/health" >/dev/null 2>&1 || \
     curl -fsS "http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}/v1/models" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$llama_pid" 2>/dev/null; then
    wait "$llama_pid" || true
    echo "llama-server exited before becoming ready" >&2
    exit 1
  fi
  if (( SECONDS >= deadline )); then
    echo "llama-server did not become ready within ${LLAMA_STARTUP_TIMEOUT}s" >&2
    kill -TERM "$llama_pid" 2>/dev/null || true
    wait "$llama_pid" || true
    exit 1
  fi
  sleep 1
done

export OPENAI_BASE_URL="${OPENAI_BASE_URL:-http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}/v1}"
export OPENAI_MODEL="${OPENAI_MODEL:-$LLAMA_MODEL_NAME}"
export OPENAI_API_KEY="${OPENAI_API_KEY:-local-llama}"

if [[ "$agent_mode" == "serve" ]]; then
  # No positional prompt was supplied, so there is nothing for the one-shot
  # agent to do.  Keep llama-server running as an OpenAI-compatible API server
  # instead of exiting with the agent's missing-prompt usage error.
  echo "no prompt argument supplied: serving llama-server only" >&2
  echo "OpenAI-compatible API: ${OPENAI_BASE_URL} (model: ${OPENAI_MODEL})" >&2
  echo "publish it with 'docker run -p 8080:8080 ...'" >&2
  echo "to run a one-shot agent request instead, append a prompt, e.g." >&2
  echo "  docker run --rm ... groovy-agent:local --workspace /output \"what is today's date?\"" >&2
  wait_for_exit_status "$llama_pid"
  exit "$wait_status"
fi

agent_args=(
  --llama-url "http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}"
  --model "$LLAMA_MODEL_NAME"
  --mcp-command /usr/local/bin/coreutils-mcp
  "$@"
)

/usr/local/bin/groovy-agent "${agent_args[@]}" <&3 &
agent_pid=$!

while true; do
  if ! kill -0 "$llama_pid" 2>/dev/null; then
    wait "$llama_pid" || true
    kill -TERM "$agent_pid" 2>/dev/null || true
    wait "$agent_pid" || true
    echo "llama-server exited unexpectedly" >&2
    exit 1
  fi
  if ! kill -0 "$agent_pid" 2>/dev/null; then
    set +e
    wait "$agent_pid"
    status=$?
    set -e
    kill -TERM "$llama_pid" 2>/dev/null || true
    wait "$llama_pid" || true
    exit "$status"
  fi
  sleep 1
done
