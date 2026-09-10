#!/usr/bin/env bash
# Deterministic container-level smoke test for the runtime Docker/Podman image.
#
# This validates packaging/wiring regressions without requiring model
# inference or downloading the GGUF model at test runtime:
#
#   1. The final image keeps this repo's container entrypoint
#      (`/usr/local/bin/entrypoint.sh`) as its effective ENTRYPOINT (not the
#      upstream llama.cpp base image entrypoint), and still contains the
#      compiled `/usr/local/bin/groovy-agent`, `/usr/local/bin/coreutils-mcp`,
#      and `/usr/local/bin/webutils-mcp` binaries plus a Chromium executable.
#   2. `docker/entrypoint.sh` starts llama-server, waits for it to become
#      healthy, and forwards the container command to `groovy-agent`
#      with the bundled MCP server configured.
#   3. Without a positional prompt, `docker/entrypoint.sh` does not invoke
#      `groovy-agent` (which is a one-shot CLI and would fail with a usage
#      error) and instead keeps llama-server serving its API until stopped.
#   4. The real `llama-server` binary spawns the bundled `coreutils-mcp` over
#      stdio from the entrypoint's `--mcp-servers-json` registration and
#      discovers its tools (this happens before the model is loaded, so a
#      placeholder model file is enough), and is also started with
#      `--ui-mcp-proxy` (mirroring groovy-sky/local-ai's
#      `LLAMA_ARG_UI_MCP_PROXY=true`) so its Web UI can reach further,
#      browser-added MCP servers.
#   5. `docker run ... mcp` serves the bundled coreutils MCP tool set over the
#      MCP Streamable HTTP transport, independently of llama-server (which is
#      not started in this mode), and completes a real `initialize` /
#      `notifications/initialized` / `tools/list` / `tools/call` (`pwd`)
#      exchange against the actual `coreutils-mcp` binary.
#   6. The bundled tool-aware chat template (docker/entrypoint.sh's default
#      `--chat-template-file`) makes the real `llama-server` binary report
#      `chat_template_caps.supports_tools`/`supports_tool_calls: true` at
#      `GET /props`, and its `/cors-proxy` endpoint confirms `--ui-mcp-proxy`
#      is actually active, using a tiny committed placeholder GGUF
#      (scripts/testdata/chat-template-smoke-model.gguf) so no ~4GB model
#      download or real text-generation inference is required.
#
# Test 2 replaces `llama-server` and `groovy-agent` inside the container with
# small deterministic stubs (a Python HTTP server that answers /health, and a
# script that records argv) so no real LLM inference happens, no model
# download is required, and no llama-server is ever exposed outside the
# container (no `-p`/published ports are used). Tests 4 and 5 use the real
# `coreutils-mcp` binary (no model/GPU/CPU inference is involved); test 5 talks to
# it with `docker exec` so its HTTP port is never published outside the
# container either. Test 6 loads the real `llama-server` binary (also reached
# only via `docker exec`, never a published port); its placeholder GGUF has a
# valid tiny architecture/tokenizer so the server starts and answers
# `/props`, but randomly initialized weights, so it is never used to
# generate text.
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

echo "==> Verifying compiled binaries and Chromium runtime"
"$CONTAINER_ENGINE" run --rm --entrypoint /bin/sh "$IMAGE_NAME" -c \
  'test -x /usr/local/bin/groovy-agent && test -x /usr/local/bin/coreutils-mcp && test -x /usr/local/bin/webutils-mcp && (command -v chromium >/dev/null || command -v chromium-browser >/dev/null || command -v google-chrome >/dev/null)'
echo "    groovy-agent, coreutils-mcp, webutils-mcp, and a Chromium/Chrome executable are present"

echo "==> Verifying runtime entrypoint wiring"
if [[ "$("$CONTAINER_ENGINE" inspect --format '{{json .Config.Entrypoint}}' "$IMAGE_NAME")" != '["/usr/local/bin/entrypoint.sh"]' ]]; then
  echo "FAIL: expected image ENTRYPOINT to be [\"/usr/local/bin/entrypoint.sh\"]" >&2
  exit 1
fi
echo "    image ENTRYPOINT is /usr/local/bin/entrypoint.sh"

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

host = os.environ.get("LLAMA_SERVER_HOST", "0.0.0.0")
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
default_llama_host="$(awk 'seen{print; exit} $0=="--host"{seen=1}' "$WORK_DIR/output/llama-argv.txt")"
if [[ "$default_llama_host" != "0.0.0.0" ]]; then
  echo "FAIL: expected entrypoint default llama-server host 0.0.0.0, got '${default_llama_host:-<missing>}'" >&2
  exit 1
fi
echo "    entrypoint passes default --host 0.0.0.0 to llama-server"
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

# Alongside the server-side MCP registration, llama-server is also started
# with its own Web UI MCP CORS proxy (--ui-mcp-proxy) by default, mirroring
# groovy-sky/local-ai's LLAMA_ARG_UI_MCP_PROXY=true. This lets the Web UI's
# browser JavaScript reach any *further* MCP servers a user registers from
# its Settings panel; the bundled coreutils tools above are unaffected either
# way, since llama-server talks to that one directly over stdio, never
# through a browser.
if ! grep -qx -- "--ui-mcp-proxy" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: expected llama-server to be started with --ui-mcp-proxy" >&2
  exit 1
fi
echo "    llama-server enables the Web UI MCP CORS proxy (--ui-mcp-proxy)"

# --ui-mcp-proxy must never be added without --tools or by weakening
# confinement: it must not be present when llama.cpp's separate, unsafe
# built-in tools feature would be enabled (it is never enabled by this
# entrypoint).
if grep -qx -- "--tools" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: entrypoint must never enable llama.cpp's built-in --tools" >&2
  exit 1
fi

# --ui-mcp-proxy has its own opt-out, independent of the coreutils
# registration, so operators can keep the bundled tools but disable the
# browser-facing CORS proxy.
EXTRA_RUN_ENV=(-e LLAMA_MCP_UI_PROXY=0)
run_forwarding_case "MCP UI proxy disabled" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
if ! grep -qx -- "--mcp-servers-json" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: LLAMA_MCP_UI_PROXY=0 must not disable the coreutils MCP server" >&2
  exit 1
fi
if grep -qx -- "--ui-mcp-proxy" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: LLAMA_MCP_UI_PROXY=0 must not add --ui-mcp-proxy" >&2
  exit 1
fi
echo "    LLAMA_MCP_UI_PROXY=0 keeps the coreutils MCP server without --ui-mcp-proxy"

# An explicit LLAMA_SERVER_HOST override must still be forwarded as llama-server's
# bind address.
EXTRA_RUN_ENV=(-e LLAMA_SERVER_HOST=127.0.0.1)
run_forwarding_case "llama host override" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
override_llama_host="$(awk 'seen{print; exit} $0=="--host"{seen=1}' "$WORK_DIR/output/llama-argv.txt")"
if [[ "$override_llama_host" != "127.0.0.1" ]]; then
  echo "FAIL: expected LLAMA_SERVER_HOST override to set --host 127.0.0.1, got '${override_llama_host:-<missing>}'" >&2
  exit 1
fi
echo "    LLAMA_SERVER_HOST override is forwarded to llama-server"

# The registration is opt-out, so operators can start llama-server without any
# tool set at all.
EXTRA_RUN_ENV=(-e LLAMA_MCP_COREUTILS=0)
run_forwarding_case "MCP registration disabled" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
if grep -qx -- "--mcp-servers-json" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: LLAMA_MCP_COREUTILS=0 must not register MCP servers" >&2
  exit 1
fi
if grep -qx -- "--ui-mcp-proxy" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: LLAMA_MCP_COREUTILS=0 must not add --ui-mcp-proxy either" >&2
  exit 1
fi
echo "    LLAMA_MCP_COREUTILS=0 starts llama-server without MCP servers or the UI proxy"

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

# --ui-mcp-proxy is passed alongside --mcp-servers-json by default (see
# docker/entrypoint.sh); confirm the real llama-server binary accepts it and
# reports the feature as enabled, rather than only asserting the argv wiring
# against the deterministic stub above.
if ! grep -q "MCP proxy (experimental)" <<< "$discovery_log"; then
  echo "FAIL: llama-server did not report the MCP proxy (--ui-mcp-proxy) as enabled" >&2
  echo "$discovery_log" >&2
  exit 1
fi
echo "    llama-server enabled the Web UI MCP CORS proxy (--ui-mcp-proxy)"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

# The model's own embedded chat template (and llama.cpp's plain "chatml"
# --chat-template fallback) do not render `tools`/`tool_calls`, so
# llama-server's jinja capability probe reports supports_tools/
# supports_tool_calls as false and the Web UI/API never receives a
# structured tool call (see the issue this addresses). docker/entrypoint.sh
# instead points --chat-template-file at a bundled tool-aware template by
# default. This is verified deterministically, without downloading or
# running inference against the real ~4GB Phi-4-mini GGUF, by loading the
# real `llama-server` binary against a committed placeholder GGUF
# (scripts/testdata/chat-template-smoke-model.gguf): a real but tiny/randomly
# initialized model whose *tokenizer and architecture* are enough for
# llama-server to start and answer GET /props, even though its untrained
# weights make it useless for actual text generation. `docker exec curl` is
# used so no port is ever published outside the container.
echo "==> Verifying the bundled chat template reports tool-call support at /props"
if ! command -v python3 >/dev/null 2>&1; then
  echo "FAIL: python3 is required on the host to parse the /props response" >&2
  exit 1
fi
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
props_startup_timeout=30
"$CONTAINER_ENGINE" run -d \
  --name "$CONTAINER_NAME" \
  -v "$ROOT_DIR/scripts/testdata/chat-template-smoke-model.gguf:/models/Phi-4-mini-instruct.Q8_0.gguf:ro" \
  -v "$WORK_DIR/output:/output" \
  -e LLAMA_CTX_SIZE=4096 \
  -e LLAMA_STARTUP_TIMEOUT="$props_startup_timeout" \
  "$IMAGE_NAME" >/dev/null

# Add slack on top of LLAMA_STARTUP_TIMEOUT for container scheduling/model
# loading overhead (image pull/start latency observed in this test
# environment is well under this), so this deadline tracks the configured
# startup timeout instead of a second, independently maintained constant.
props_deadline_slack_seconds=30
props_deadline=$((SECONDS + props_startup_timeout + props_deadline_slack_seconds))
props_response=""
until [[ -n "$props_response" ]]; do
  if (( SECONDS >= props_deadline )); then
    echo "FAIL: llama-server did not become ready to serve /props in time" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  if [[ "$("$CONTAINER_ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null)" != "true" ]]; then
    echo "FAIL: llama-server container exited before serving /props" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  props_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" \
    curl -fsS "http://127.0.0.1:8080/props" 2>/dev/null || true)"
  [[ -n "$props_response" ]] || sleep 1
done

if ! props_caps="$(python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
    caps = doc["chat_template_caps"]
    ok = bool(caps.get("supports_tools")) and bool(caps.get("supports_tool_calls"))
except Exception as exc:
    print(f"error parsing /props: {exc}", file=sys.stderr)
    sys.exit(1)
print(json.dumps(caps))
sys.exit(0 if ok else 1)
' <<< "$props_response")"; then
  echo "FAIL: /props does not report chat_template_caps.supports_tools/supports_tool_calls: true" >&2
  echo "$props_response" >&2
  exit 1
fi
echo "    /props reports chat_template_caps: $props_caps"

# With MCP enabled by default, /tools must be active (not feature_disabled)
# and expose the bundled coreutils tool names consumed by the built-in Web UI.
tools_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" \
  curl -fsS "http://127.0.0.1:8080/tools" 2>/dev/null || true)"
if [[ -z "$tools_response" ]]; then
  echo "FAIL: /tools returned an empty response" >&2
  exit 1
fi
if ! tools_count="$(python3 -c '
import json, sys
doc = json.load(sys.stdin)
if isinstance(doc, dict):
    if doc.get("error", {}).get("type") == "feature_disabled":
        print("feature_disabled", file=sys.stderr)
        sys.exit(1)
    items = doc.get("data")
elif isinstance(doc, list):
    items = doc
else:
    items = None
if not isinstance(items, list):
    print("missing tools list", file=sys.stderr)
    sys.exit(1)
count = sum(1 for item in items if isinstance(item, dict) and str(item.get("tool", "")).startswith("coreutils_"))
if count <= 0:
    print("no coreutils tools discovered", file=sys.stderr)
    sys.exit(1)
print(count)
' <<< "$tools_response")"; then
  echo "FAIL: /tools is not enabled with bundled MCP tools" >&2
  echo "$tools_response" >&2
  exit 1
fi
echo "    /tools is enabled and advertises $tools_count bundled coreutils tool(s)"

# --ui-mcp-proxy makes llama-server serve a `/cors-proxy` endpoint for the Web
# UI's own MCP-over-browser feature; a disabled proxy answers 403 there (see
# LLAMA_MCP_UI_PROXY=0 in docker/entrypoint.sh), while an enabled one accepts
# the request path and fails later for unrelated reasons (e.g. a missing/
# invalid target), so any non-403 status is enough to confirm the capability
# is actually wired up in the real binary, without needing a real upstream
# MCP server to proxy to.
cors_proxy_status="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" \
  curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8080/cors-proxy" 2>/dev/null || true)"
if [[ "$cors_proxy_status" == "403" || -z "$cors_proxy_status" ]]; then
  echo "FAIL: /cors-proxy reported status '$cors_proxy_status'; expected --ui-mcp-proxy to be enabled" >&2
  exit 1
fi
echo "    /cors-proxy responds (status $cors_proxy_status), confirming --ui-mcp-proxy is enabled"
"$CONTAINER_ENGINE" stop -t 15 "$CONTAINER_NAME" >/dev/null 2>&1 || true
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
