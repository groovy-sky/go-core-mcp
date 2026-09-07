#!/usr/bin/env bash
# Deterministic container-level smoke test for the runtime Docker/Podman image.
#
# This validates packaging/wiring regressions without requiring model
# inference or downloading the GGUF model at test runtime:
#
#   1. The compiled `/usr/local/bin/groovy-agent` and
#      `/usr/local/bin/coreutils-mcp` binaries are present in the final image.
#   2. `docker/entrypoint.sh` starts llama-server, waits for it to become
#      healthy, and forwards the container command to `groovy-agent`
#      with the bundled MCP server configured.
#   3. Without a positional prompt, `docker/entrypoint.sh` does not invoke
#      `groovy-agent` (which is a one-shot CLI and would fail with a usage
#      error) and instead keeps llama-server serving its API until stopped.
#   4. The real `llama-server` binary spawns the bundled `coreutils-mcp` over
#      stdio from the entrypoint's `--mcp-servers-json` registration and
#      discovers its tools (this happens before the model is loaded, so a
#      placeholder model file is enough).
#   5. `docker run ... mcp` serves the bundled coreutils MCP tool set over the
#      MCP Streamable HTTP transport, independently of llama-server (which is
#      not started in this mode), and completes a real `initialize` /
#      `notifications/initialized` / `tools/list` / `tools/call` (`pwd`)
#      exchange against the actual `coreutils-mcp` binary.
#
# Test 2 replaces `llama-server` and `groovy-agent` inside the container with
# small deterministic stubs (a Python HTTP server that answers /health, and a
# script that records argv) so no real LLM inference happens, no model
# download is required, and no llama-server is ever exposed outside the
# container (no `-p`/published ports are used). Tests 4 and 5 use the real
# `coreutils-mcp` binary (no model/GPU/CPU inference is involved); test 5 talks to
# it with `docker exec` so its HTTP port is never published outside the
# container either.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE_NAME="${IMAGE_NAME:-groovy-agent:smoke-test}"
CONTAINER_ENGINE="${CONTAINER_ENGINE:-docker}"

WORK_DIR="$(mktemp -d)"
cleanup() {
  status=$?
  # Best-effort: stop/remove any smoke-test container left running and drop
  # the scratch directory used for stub binaries and captured output.
  "$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  rm -rf "$WORK_DIR"
  exit "$status"
}
trap cleanup EXIT INT TERM

CONTAINER_NAME="groovy-agent-smoke-$$"

echo "==> Building runtime image (no model download)"
DOCKER_BUILDKIT=1 "$CONTAINER_ENGINE" build \
  --target runtime \
  --build-arg DOWNLOAD_MODEL=0 \
  -t "$IMAGE_NAME" \
  "$ROOT_DIR"

echo "==> Verifying compiled binaries"
"$CONTAINER_ENGINE" run --rm --entrypoint /bin/sh "$IMAGE_NAME" -c \
  'test -x /usr/local/bin/groovy-agent && test -x /usr/local/bin/coreutils-mcp'
echo "    groovy-agent and coreutils-mcp binaries OK"

echo "==> Verifying llama-server host default"
if ! "$CONTAINER_ENGINE" inspect \
    --format '{{range .Config.Env}}{{println .}}{{end}}' "$IMAGE_NAME" | grep -qx 'LLAMA_SERVER_HOST=0.0.0.0'; then
  echo "FAIL: expected image default LLAMA_SERVER_HOST=0.0.0.0" >&2
  exit 1
fi
echo "    LLAMA_SERVER_HOST defaults to 0.0.0.0"

mkdir -p "$WORK_DIR/output"

cat > "$WORK_DIR/stub-llama-server" <<'EOF'
#!/usr/bin/env python3
"""Deterministic stand-in for llama-server used by the smoke test.

Records the CLI args it was started with (so the entrypoint's llama-server
wiring can be asserted) and serves a minimal HTTP server that answers /health
and /v1/models with HTTP 200 so docker/entrypoint.sh's readiness loop
succeeds without requiring a real model, GPU, or CPU inference.
"""
import http.server
import os
import sys

with open("/output/llama-argv.txt", "w", encoding="utf-8") as fp:
    fp.write("\n".join(sys.argv[1:]) + "\n")

host = os.environ.get("LLAMA_SERVER_HOST", "127.0.0.1")
port = int(os.environ.get("LLAMA_SERVER_PORT", "8080"))


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path in ("/health", "/v1/models"):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"{}")
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, fmt, *args):
        pass


http.server.HTTPServer((host, port), Handler).serve_forever()
EOF

cat > "$WORK_DIR/stub-groovy-agent" <<'EOF'
#!/usr/bin/env bash
# Deterministic stand-in for groovy-agent that records the argv forwarded by
# docker/entrypoint.sh instead of performing real LLM inference.
printf '%s\n' "$@" > /output/forward-log.txt
exit 0
EOF

chmod +x "$WORK_DIR/stub-llama-server" "$WORK_DIR/stub-groovy-agent"
touch "$WORK_DIR/fake-model.gguf"

run_forwarding_case() {
  local case_name="$1"
  shift
  rm -f "$WORK_DIR/output/forward-log.txt" "$WORK_DIR/output/llama-argv.txt"
  echo "==> Verifying entrypoint startup/command forwarding ($case_name)"
  "$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  timeout 60 "$CONTAINER_ENGINE" run --rm \
    --name "$CONTAINER_NAME" \
    -v "$WORK_DIR/stub-llama-server:/opt/llama/llama-server:ro" \
    -v "$WORK_DIR/stub-groovy-agent:/usr/local/bin/groovy-agent:ro" \
    -v "$WORK_DIR/fake-model.gguf:/models/Phi-4-mini-instruct.Q8_0.gguf:ro" \
    -v "$WORK_DIR/output:/output" \
    -e LLAMA_STARTUP_TIMEOUT=15 \
    ${EXTRA_RUN_ENV[@]+"${EXTRA_RUN_ENV[@]}"} \
    "$IMAGE_NAME" "$@"

  if [[ ! -f "$WORK_DIR/output/forward-log.txt" ]]; then
    echo "FAIL: entrypoint did not forward to groovy-agent ($case_name)" >&2
    exit 1
  fi
  echo "    forwarded argv: $(tr '\n' ' ' < "$WORK_DIR/output/forward-log.txt")"
}

EXTRA_RUN_ENV=()

# The entrypoint provides container defaults before user-supplied agent flags and
# prompt arguments.
run_forwarding_case "agent defaults plus prompt" --workspace /output "test prompt"
if ! grep -qx -- "--mcp-command" "$WORK_DIR/output/forward-log.txt"; then
  echo "FAIL: expected bundled MCP command flag" >&2
  exit 1
fi
if ! grep -qx -- "/usr/local/bin/coreutils-mcp" "$WORK_DIR/output/forward-log.txt"; then
  echo "FAIL: expected bundled MCP command path" >&2
  exit 1
fi
if ! tail -n1 "$WORK_DIR/output/forward-log.txt" | grep -qx "test prompt"; then
  echo "FAIL: expected prompt to be forwarded" >&2
  exit 1
fi

# llama-server itself is started as an MCP client: the bundled read-only
# coreutils server is registered with it over stdio, confined to the workspace.
if ! grep -qx -- "--mcp-servers-json" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: expected llama-server to be started with --mcp-servers-json" >&2
  exit 1
fi
if ! grep -q '"command":"/usr/local/bin/coreutils-mcp"' "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: expected the coreutils MCP server in the llama-server MCP config" >&2
  exit 1
fi
if ! grep -q '"--workspace","/output"' "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: expected the MCP config to confine tools to the workspace" >&2
  exit 1
fi
echo "    llama-server registers the bundled coreutils MCP server over stdio"

# The registration is opt-out, so operators can start llama-server without any
# tool set at all.
EXTRA_RUN_ENV=(-e LLAMA_MCP_COREUTILS=0)
run_forwarding_case "MCP registration disabled" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
if grep -qx -- "--mcp-servers-json" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: LLAMA_MCP_COREUTILS=0 must not register MCP servers" >&2
  exit 1
fi
echo "    LLAMA_MCP_COREUTILS=0 starts llama-server without MCP servers"

# Without a positional prompt there is nothing for the one-shot agent to do, so
# the entrypoint must keep llama-server running as an API server instead of
# invoking groovy-agent and failing with its missing-prompt usage error.
echo "==> Verifying no-prompt serve-only mode"
rm -f "$WORK_DIR/output/forward-log.txt"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_ENGINE" run -d \
  --name "$CONTAINER_NAME" \
  -v "$WORK_DIR/stub-llama-server:/opt/llama/llama-server:ro" \
  -v "$WORK_DIR/stub-groovy-agent:/usr/local/bin/groovy-agent:ro" \
  -v "$WORK_DIR/fake-model.gguf:/models/Phi-4-mini-instruct.Q8_0.gguf:ro" \
  -v "$WORK_DIR/output:/output" \
  -e LLAMA_STARTUP_TIMEOUT=15 \
  "$IMAGE_NAME" >/dev/null

serve_deadline=$((SECONDS + 60))
until "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" 2>&1 | grep -q "serving llama-server only"; do
  if (( SECONDS >= serve_deadline )); then
    echo "FAIL: entrypoint did not announce serve-only mode" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  sleep 1
done

if [[ "$("$CONTAINER_ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME")" != "true" ]]; then
  echo "FAIL: container exited instead of serving llama-server" >&2
  "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
  exit 1
fi

if [[ -f "$WORK_DIR/output/forward-log.txt" ]]; then
  echo "FAIL: groovy-agent was invoked without a prompt" >&2
  exit 1
fi

if ! "$CONTAINER_ENGINE" exec "$CONTAINER_NAME" \
    curl -fsS "http://127.0.0.1:8080/v1/models" >/dev/null; then
  echo "FAIL: llama-server API not reachable in serve-only mode" >&2
  exit 1
fi
echo "    llama-server kept serving and groovy-agent was not invoked"

"$CONTAINER_ENGINE" stop -t 15 "$CONTAINER_NAME" >/dev/null
serve_exit="$("$CONTAINER_ENGINE" inspect -f '{{.State.ExitCode}}' "$CONTAINER_NAME")"
case "$serve_exit" in
  0|143) ;;
  *)
    echo "FAIL: serve-only container exited with unexpected status $serve_exit" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
    ;;
esac
echo "    serve-only container shut down cleanly on SIGTERM (exit $serve_exit)"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

# The bundled llama.cpp build is itself an MCP client: it spawns the servers
# listed in --mcp-servers-json over stdio and registers their tools before it
# loads the model. Running the *real* llama-server against a placeholder model
# file therefore exercises the full discovery handshake (initialize /
# notifications/initialized / tools/list) against the real coreutils-mcp
# binary, and still needs no model download, GPU, or inference: llama-server
# fails right after discovery, when it tries to load the placeholder model.
echo "==> Verifying llama-server discovers the bundled coreutils MCP tools"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
set +e
discovery_log="$(timeout 120 "$CONTAINER_ENGINE" run --rm \
  --name "$CONTAINER_NAME" \
  -v "$WORK_DIR/fake-model.gguf:/models/Phi-4-mini-instruct.Q8_0.gguf:ro" \
  -v "$WORK_DIR/output:/output" \
  -e LLAMA_STARTUP_TIMEOUT=15 \
  "$IMAGE_NAME" 2>&1)"
discovery_status=$?
set -e
if (( discovery_status == 124 )); then
  echo "FAIL: llama-server did not exit within the discovery timeout" >&2
  echo "$discovery_log" >&2
  exit 1
fi

if ! grep -qE "MCP warmup: 'coreutils' discovered [1-9][0-9]* tools" <<< "$discovery_log"; then
  echo "FAIL: llama-server did not discover any coreutils MCP tools" >&2
  echo "$discovery_log" >&2
  exit 1
fi
if ! grep -qE "Added [1-9][0-9]* MCP tools" <<< "$discovery_log"; then
  echo "FAIL: llama-server did not register the discovered MCP tools" >&2
  echo "$discovery_log" >&2
  exit 1
fi
echo "    llama-server spawned coreutils-mcp over stdio and registered its tools"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

# The `mcp` subcommand is a third, explicit deployment mode: it serves the
# bundled read-only coreutils MCP tool set over the MCP Streamable HTTP
# transport, independently of llama-server (which is not started at all in
# this mode). This exercises the real `coreutils-mcp` binary end to end
# (initialize, tools/list, tools/call) without any model/GPU/CPU inference,
# using `docker exec` so no port is ever published outside the container.
echo "==> Verifying remote MCP server mode (docker run ... mcp)"
rm -f "$WORK_DIR/output/forward-log.txt"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_ENGINE" run -d \
  --name "$CONTAINER_NAME" \
  -v "$WORK_DIR/output:/output" \
  "$IMAGE_NAME" mcp >/dev/null

mcp_deadline=$((SECONDS + 30))
until "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" 2>&1 | grep -q "listening for MCP Streamable HTTP"; do
  if (( SECONDS >= mcp_deadline )); then
    echo "FAIL: coreutils-mcp did not report it was listening" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  if [[ "$("$CONTAINER_ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null)" != "true" ]]; then
    echo "FAIL: mcp container exited before becoming ready" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  sleep 1
done

if ! "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" 2>&1 | grep -q "WARNING: MCP_HTTP_TOKEN is not set"; then
  echo "FAIL: expected an unauthenticated-exposure warning without MCP_HTTP_TOKEN" >&2
  exit 1
fi
echo "    coreutils-mcp is listening and warns about the missing bearer token"

initialize_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" curl -fsS \
  -X POST "http://127.0.0.1:8765/mcp" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke-test","version":"1"}}}')"
if [[ "$initialize_response" != *'"protocolVersion"'* ]]; then
  echo "FAIL: initialize did not return a protocol version: $initialize_response" >&2
  exit 1
fi

"$CONTAINER_ENGINE" exec "$CONTAINER_NAME" curl -fsS \
  -X POST "http://127.0.0.1:8765/mcp" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null

tools_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" curl -fsS \
  -X POST "http://127.0.0.1:8765/mcp" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')"
if [[ "$tools_response" != *'"pwd"'* ]]; then
  echo "FAIL: tools/list did not advertise the pwd tool: $tools_response" >&2
  exit 1
fi

pwd_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" curl -fsS \
  -X POST "http://127.0.0.1:8765/mcp" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pwd","arguments":{}}}')"
if [[ "$pwd_response" != *'\"success\":true'* ]]; then
  echo "FAIL: pwd tool call did not succeed: $pwd_response" >&2
  exit 1
fi
echo "    initialize / tools/list / tools/call (pwd) succeeded over Streamable HTTP"

"$CONTAINER_ENGINE" stop -t 15 "$CONTAINER_NAME" >/dev/null
mcp_exit="$("$CONTAINER_ENGINE" inspect -f '{{.State.ExitCode}}' "$CONTAINER_NAME")"
case "$mcp_exit" in
  0|143) ;;
  *)
    echo "FAIL: mcp container exited with unexpected status $mcp_exit" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
    ;;
esac
echo "    mcp container shut down cleanly on SIGTERM (exit $mcp_exit)"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

echo "==> Container smoke test passed"
