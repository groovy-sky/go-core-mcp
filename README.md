# groovy-agent

A minimal, reliable Go agent that answers a single prompt using a local
[`llama.cpp`](https://github.com/ggml-org/llama.cpp) `llama-server` and a
bounded set of **workspace and text tools** exposed over the
[Model Context Protocol (MCP)](https://modelcontextprotocol.io/).

The design intentionally has no non-coreutils integrations: no network
fetch, no browser, no GitHub/cloud APIs, no package management, no shell
string execution, and no arbitrary command runner or unrestricted file
mutation. See
[Architecture](#architecture) and [Security boundaries](#security-boundaries)
below for the exact guarantees.

## Model

The bundled/default model is
[`Phi-4-mini-instruct.Q8_0.gguf`](https://huggingface.co/unsloth/Phi-4-mini-instruct-GGUF/blob/main/Phi-4-mini-instruct.Q8_0.gguf)
from `unsloth/Phi-4-mini-instruct-GGUF`, served locally by `llama-server`.
The GGUF file is **never committed to git**; it is downloaded at
build/run time (see below).

## Architecture

```text
User prompt
   │
   ▼
Go CLI agent (cmd/agent)
   ├── HTTP  ─────► llama-server (OpenAI-compatible /v1/chat/completions)
   │                Phi-4-mini-instruct GGUF, llama.cpp
   │
   └── stdio ────► coreutils MCP server (cmd/coreutils-mcp)
```

`cmd/coreutils-mcp` can alternatively be run in a second, independent
mode that serves the same bounded tool set over the network for any
remote MCP-compatible client, instead of being spawned as the agent's
stdio child process:

```text
Remote MCP client ── MCP Streamable HTTP ──► coreutils MCP server (cmd/coreutils-mcp --transport http)
```

This does **not** go through `llama-server` or the Go agent at all; it is
a standalone deployment of the coreutils MCP server for clients that speak
MCP natively (see [MCP server standalone mode](#mcp-server-standalone-mode)
below).

Inside the Docker image there is a third wiring: the bundled `llama-server`
is itself an MCP client. It spawns the coreutils MCP server over stdio and
exposes its tools to the chat flow (including its own Web UI), so no
external MCP client is needed:

```text
Chat client / llama.cpp Web UI ──► llama-server ── stdio ──► coreutils MCP server
```

See [Coreutils tools inside llama.cpp](#coreutils-tools-inside-llamacpp)
below.

The agent (`internal/agent`):

1. Validates configuration (workspace must exist; URLs must be http/https).
2. Connects to `llama-server` and to the coreutils MCP server (a child
   process started over stdio).
3. Performs MCP `initialize` / `tools/list` and keeps only tools on the
   built-in allowlist (`internal/agent/agent.go: AllowedTools`); anything
   else the MCP server might advertise is logged and rejected.
4. Picks a small, deterministic tool profile for the prompt
   (`internal/agent/profiles.go`) so only a handful of relevant tool
   schemas are sent to the model at once (max 6).
5. Runs a bounded loop (at most 3 model rounds, 5 total tool calls): the
   model may request tool calls, the agent validates and executes them
   through MCP, and bounded results are fed back until the model returns a
   final answer or the round budget is spent.
6. Prints the final answer to stdout; all diagnostics go to stderr.

There is no shell execution, no free-form command string, and no
long-running session state. All filesystem access goes through dedicated,
workspace-confined tools. Each run answers exactly one prompt and exits.

## Supported MCP tools

The MCP server exposes exactly one closed-schema tool, `coreutils_run`:

```json
{"command":"sort","args":["--reverse"],"stdin":"pear\napple\n"}
```

It runs only approved, in-process, read-only text commands against the supplied
stdin: `sort` (`-r`, `--reverse`, `-n`, `--numeric-sort`), `uniq` (`-c`,
`--count`), `wc` (`-l`, `-w`, `-c`), `tr`, `head -n COUNT`, `tail -n COUNT`,
and `cut -d DELIMITER -f FIELDS`. The response contains `command`, `stdout`,
`stderr`, and `truncated`. Unknown commands and unsupported arguments are
rejected before execution. The agent independently allowlists this same tool.

It also exposes dedicated workspace tools:

- `pwd`, `ls`
- `cat`, `read_file`, `head`, `tail`
- `grep`, `grep_file`, `grep_text`
- `find`
- `touch`, `write_file`, `mkdir`, `cp`, `mv`, `rm`, `rmdir`

`read_file` and `cat` read at most the requested `max_bytes` (capped at
12 KiB) and reject non-UTF-8/binary data. `grep_file` searches only the
first 12 KiB of a text file and returns `line:text` matches with a
`truncated` flag when the file or result set exceeds the budget;
`grep_text` applies the same matching options (`fixed`, `ignore_case`,
`max_matches`) to in-memory text. `find` recursively walks from an
optional workspace-relative root path (default `.`), matches
workspace-relative paths using the same pattern options, can include files
and/or directories, returns at most 20 matches, and skips unreadable
subtrees while reporting the skip count in metadata. `write_file` creates a
missing file beneath an existing workspace directory, or replaces and
truncates an existing regular file only when `overwrite: true` is set.
Copy is capped at 1 MiB, `rm` only removes one regular file, and `rmdir`
only removes an empty directory. All paths are relative to the configured
workspace; absolute paths, traversal, and symlink escapes are rejected.

## Security boundaries

- **No shell execution.** Every tool is a Go function operating on parsed,
  schema-validated arguments; there is no `sh -c`, `exec.Command` with a
  shell, or string concatenation into a command line anywhere in the tool
  dispatch path.
- **No shell or network access.** The tools never invoke a subprocess or
  contact the network. Filesystem access is limited to dedicated,
  schema-validated workspace tools.
- **Bounded I/O.** Input is capped at 64 KiB, each argument at 4 KiB (up to
  32 arguments), stdout at 256 KiB, and every call is subject to the server
  deadline.
- **Allowlist, not trust-the-server.** The agent filters MCP `tools/list`
  results against its own hard-coded `AllowedTools`, so even if the MCP
  server were modified or replaced, the agent will not send unexpected
  tool schemas to the model or execute unexpected tool calls.
- **Local-only inference.** `llama-server` is only reachable via
  `--llama-url`, which must be an `http://` or `https://` URL; the
  container entrypoint wires this to the colocated `llama-server`
  endpoint (`LLAMA_SERVER_HOST` defaults to `0.0.0.0`) and never forwards
  it to an external API.
- **No arbitrary mutation tools.** There is no `apply_patch`,
  `exec_command`, or arbitrary command runner in this design. Writes are
  limited to dedicated, workspace-confined tools such as `write_file`,
  `touch`, `mkdir`, `cp`, `mv`, `rm`, and `rmdir`.

## Prerequisites

- Go 1.24 or newer (for building/testing the Go binaries directly).
- Docker (or Podman) with BuildKit, if you want the containerized stack
  that bundles `llama-server`.
- `curl`, for the model download script.
- ~4.5 GB disk space for `Phi-4-mini-instruct.Q8_0.gguf`, plus the
  Docker image layers if using the container.

## Build and test (Go only)

```sh
go build ./...
go vet ./...
go test ./...
```

This builds two binaries from `cmd/`:

- `cmd/agent` → the CLI agent (`groovy-agent`)
- `cmd/coreutils-mcp` → the standalone coreutils MCP server

The implementation has no third-party runtime or build dependencies.

## Model download (no GGUF committed to git)

```sh
./scripts/download-model.sh
# optional token for gated/rate-limited downloads:
# HF_TOKEN=... ./scripts/download-model.sh
```

This stores `artifacts/models/Phi-4-mini-instruct.Q8_0.gguf`. `*.gguf`
files are ignored by git (see `.gitignore`).

## Running locally without Docker

1. Download the model (above).
2. Start `llama-server` yourself (from a local `llama.cpp` build or
   release) pointed at the downloaded GGUF file, e.g.:

   ```sh
   llama-server \
     --host 127.0.0.1 --port 8080 \
     --model artifacts/models/Phi-4-mini-instruct.Q8_0.gguf \
     --alias Phi-4-mini-instruct \
     --ctx-size 8192 --jinja
   ```

3. Build the binaries and run the agent, pointing it at the MCP server
   binary and a workspace directory:

   ```sh
   go build -o bin/coreutils-mcp ./cmd/coreutils-mcp
   go build -o bin/groovy-agent ./cmd/agent
   ./bin/groovy-agent \
     --llama-url http://127.0.0.1:8080 \
     --model Phi-4-mini-instruct \
     --mcp-command ./bin/coreutils-mcp \
     --workspace . \
     "what is the sha256sum of go.mod?"
   ```

## Running the Docker image (llama.cpp + agent bundled)

The `Dockerfile` builds both Go binaries, layers them on top of the
official `llama.cpp` server image, and wires everything together with
`docker/entrypoint.sh`, which starts `llama-server`, waits for it to
become healthy, then runs `groovy-agent` with the bundled MCP server
configured.

When using the published image from GHCR, the Phi-4 workflow now pushes:

- `ghcr.io/groovy-sky/groovy-agent:phi4-mini` (moving tag), plus
- immutable tags `ghcr.io/groovy-sky/groovy-agent:phi4-mini-<git-sha>` and
  `ghcr.io/groovy-sky/groovy-agent:phi4-mini-run-<workflow-run-number>`.

For deterministic deployments and debugging, prefer one of the immutable
tags (or a digest) so you never pull a stale mutable manifest by accident.

`groovy-agent` is a one-shot CLI, so the container behaves differently
depending on the command it is given:

- **with a prompt** (`docker run ... groovy-agent:local "what is today's
  date?"`): the agent answers that single request, prints the answer on
  stdout, and the container exits with the agent's exit status;
- **without a prompt** (`docker run ... groovy-agent:local`): there is
  nothing for the one-shot agent to do, so the entrypoint skips it and
  keeps `llama-server` running as an OpenAI-compatible API server until
  the container is stopped;
- **with `mcp` as the first argument** (`docker run ... groovy-agent:local
  mcp`): the entrypoint does not start `llama-server` or the agent at
  all; it runs `coreutils-mcp --transport http`, serving the bundled
  bounded workspace/text tool set over the MCP Streamable HTTP transport for
  any remote MCP-compatible client (see [Run the remote MCP server](#run-the-remote-mcp-server-no-llama-server)
  below).

### Build with the model baked into the image

```sh
HF_TOKEN=... DOWNLOAD_MODEL_AT_BUILD=1 ./scripts/package-image.sh
```

(`HF_TOKEN` is optional and only needed for gated/rate-limited downloads.)
This produces a local image (`groovy-agent:local` by default) and saves a
tarball to `output/groovy-agent.tar`.

### Build without downloading the model (mount it instead)

```sh
DOCKER_BUILDKIT=1 docker build -t groovy-agent:local .
```

Then run with the host-downloaded model mounted read-only:

```sh
docker run --rm \
  -v "$(pwd)/artifacts/models:/models:ro" \
  -e LLAMA_MODEL_PATH=/models/Phi-4-mini-instruct.Q8_0.gguf \
  groovy-agent:local \
  --workspace /output "what is today's date?"
```

### Run as an API server (no prompt)

Omit the prompt to keep `llama-server` running instead of answering a
single request. The image defaults to binding `llama-server` to all
container interfaces so the published port is reachable from the host:

```sh
docker run --rm \
  -v "$(pwd)/artifacts/models:/models:ro" \
  -e LLAMA_MODEL_PATH=/models/Phi-4-mini-instruct.Q8_0.gguf \
  -p 8080:8080 \
  groovy-agent:local
```

The OpenAI-compatible API is then available at
`http://127.0.0.1:8080/v1` on the host (model name
`Phi-4-mini-instruct`). `llama-server` uses no API key, so only expose it
on trusted networks. Because the bundled coreutils MCP server is
registered with it (see below), `llama-server` restricts CORS origins to
localhost; pass `LLAMA_EXTRA_ARGS="--cors-origins <origin>"` if a browser
served from another origin has to reach it, or disable the tool set with
`LLAMA_MCP_COREUTILS=0`.

### Coreutils tools inside llama.cpp

The pinned `llama.cpp` build (`build 10481`, commit `25ae3a9b3`) is an MCP
client itself: it spawns the MCP servers listed in `--mcp-servers-json`
(Cursor-compatible format) over **stdio**, discovers their tools at
startup and offers them to the model through its OpenAI-compatible tool
calling, exposing them on its internal `GET /tools` endpoint that the
built-in Web UI consumes.

`docker/entrypoint.sh` therefore registers the bundled read-only
`coreutils-mcp` binary with `llama-server` automatically whenever
`llama-server` is started (both the API-server mode and the one-shot
prompt mode). Nothing extra has to be started or published, and the
startup logs show the discovery:

```text
srv start: MCP warmup: 'coreutils' discovered 18 tools
srv setup: Added 18 MCP tools
```

The tools then appear in the built-in Web UI's tool list as
`coreutils_pwd`, `coreutils_read_file`, `coreutils_write_file`, ... and can
be enabled per chat.

Usage and limitations:

- The tools are registered **server-side**: `llama-server` spawns and talks
  to the bundled `coreutils-mcp` over stdio itself and exposes what it
  discovers on `GET /tools`, so the model (and the built-in Web UI, which
  reads that same endpoint) can call these tools without the browser ever
  reaching the MCP server directly. `llama-server`'s own MCP client in this
  build supports the **stdio** transport only — it cannot connect to the
  Streamable HTTP endpoint served by `docker run ... mcp`. That standalone
  mode remains for external MCP-capable clients.
- The entrypoint also starts `llama-server` with `--ui-mcp-proxy` by
  default, mirroring `groovy-sky/local-ai`'s
  `LLAMA_ARG_UI_MCP_PROXY=true`. This is a separate, Web-UI-only feature:
  it starts a `/cors-proxy` endpoint so the Web UI's own browser
  JavaScript can reach *additional* MCP servers a user registers directly
  from its Settings panel (something a browser cannot otherwise do
  cross-origin). It has no effect on the bundled `coreutils-mcp`
  registration above — that one is discovered and exposed the same way
  with or without `--ui-mcp-proxy` — but is enabled by default for parity
  with the reference `local-ai` configuration and so the Web UI's MCP
  Settings panel is fully usable out of the box. Set
  `LLAMA_MCP_UI_PROXY=0` to start `llama-server` without it while keeping
  the bundled coreutils tools.
- Tool calling requires a chat template with tool support; the entrypoint
  always starts `llama-server` with `--jinja`. Neither the bundled
  Phi-4-mini GGUF's own embedded chat template nor llama.cpp's plain
  `chatml` `--chat-template` fallback render `tools`/`tool_calls`
  (`GET /props` reports `chat_template_caps.supports_tools`/
  `supports_tool_calls: false` for both), so without further action the
  model free-generates pseudo-shell text like `coreutil_pwd --show-path`
  instead of making a structured tool call. The entrypoint instead
  defaults to a bundled tool-aware template — see
  `LLAMA_CHAT_TEMPLATE_FILE` below.
- MCP support in llama.cpp is marked experimental upstream, and enabling
  it limits CORS origins to localhost by default (see above).
- Only the bundled bounded tool set is registered. llama.cpp's own
  built-in tools (`--tools`, which include `write_file` and
  `exec_shell_command`) are deliberately **not** enabled, and every
  registered tool stays confined to `LLAMA_MCP_WORKSPACE` (`/output` by
  default), exactly as in the standalone `mcp` mode.
- `llama-server` owns the lifetime of the MCP child process: it spawns it
  on demand and shuts it down when it exits, so stopping the container
  leaves nothing behind.
- Set `LLAMA_MCP_COREUTILS=0` to start `llama-server` without any MCP tool
  set, and `LLAMA_MCP_WORKSPACE=/some/dir` to confine the tools to a
  different mounted directory.

```sh
docker run --rm \
  -v "$(pwd)/artifacts/models:/models:ro" \
  -v "$(pwd)/output:/output" \
  -e LLAMA_MODEL_PATH=/models/Phi-4-mini-instruct.Q8_0.gguf \
  -e LLAMA_MCP_WORKSPACE=/output \
  -p 8080:8080 \
  groovy-agent:local
```

### Output persistence

Mount `/output` as the workspace when you want file-reading tools to see
files written by the host:

```sh
docker run --rm \
  -v "$(pwd)/artifacts/models:/models:ro" \
  -v "$(pwd)/output:/output" \
  -e LLAMA_MODEL_PATH=/models/Phi-4-mini-instruct.Q8_0.gguf \
  groovy-agent:local \
  --workspace /output "summarize the first lines of README.md"
```

### Run the remote MCP server (no llama-server)

Pass `mcp` as the container command to serve the bundled read-only
coreutils tool set over the
[MCP Streamable HTTP transport](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#streamable-http)
for any remote MCP-compatible client. This mode does not start
`llama-server` or `groovy-agent`; it is independent of, and can be
published separately from, the OpenAI-compatible API:

```sh
docker run --rm \
  -v "$(pwd)/output:/output" \
  -p 8765:8765 \
  -e MCP_HTTP_TOKEN=change-me \
  groovy-agent:local \
  mcp
```

The MCP endpoint is then `http://127.0.0.1:8765/mcp` on the host. **Do
not** point llama.cpp's built-in web UI at this endpoint: that UI cannot
be told about an MCP server at runtime, and the llama.cpp MCP client in
the pinned build speaks the stdio transport only, so it will never
connect to this HTTP endpoint no matter which port you give it. To use
the coreutils tools from llama.cpp, use the built-in registration
described in
[Coreutils tools inside llama.cpp](#coreutils-tools-inside-llamacpp)
instead. For any other MCP-capable client, connect directly to the
Streamable HTTP endpoint, for example:

```json
{
  "mcpServers": {
    "coreutils": {
      "type": "http",
      "url": "http://127.0.0.1:8765/mcp",
      "headers": {
        "Authorization": "<AUTH_HEADER_VALUE>"
      }
    }
  }
}
```

Set the header value to the word "Bearer" followed by a space and the
`MCP_HTTP_TOKEN` value (omit the `headers` block entirely if you did not
set `MCP_HTTP_TOKEN`).

Security implications of publishing this port:

- **Set `MCP_HTTP_TOKEN`** (or `--http-token` if running `coreutils-mcp`
  directly) whenever the port is reachable from anything other than a
  fully trusted local network. Without it, every request is accepted
  unauthenticated: the entrypoint logs a startup warning to remind you,
  and the tool set is read-only/workspace-confined but still lets any
  reachable caller read files under the mounted workspace.
- The image binds `MCP_HTTP_HOST=0.0.0.0` by default so `-p` publishing
  works (as with `LLAMA_SERVER_HOST`, above); it is Docker's `-p` mapping,
  not the bind address, that controls whether the port is reachable from
  outside the container. Omit `-p` to keep the endpoint host-only.
  `cmd/coreutils-mcp` run directly on a host (outside Docker) instead
  defaults `--listen` to `127.0.0.1:8765` (loopback-only) precisely so it
  is not reachable from the network unless you explicitly rebind it.
- Mount only the directory you intend to expose as `/output`; every tool
  call is confined to that workspace root regardless of the request.

Configuration for this mode (env vars, all optional):

- `MCP_HTTP_HOST` (default `0.0.0.0`)
- `MCP_HTTP_PORT` (default `8765`)
- `MCP_HTTP_PATH` (default `/mcp`)
- `MCP_HTTP_TOKEN` (default unset/unauthenticated; see above)
- `MCP_WORKSPACE` (default `${AGENT_OUTPUT_DIR:-/output}`)

Any extra arguments after `mcp` are forwarded to `coreutils-mcp` and can
override these, e.g. `docker run ... groovy-agent:local mcp --http-path
/coreutils`.

### Container smoke test

Validate the runtime image's packaging and `docker/entrypoint.sh` wiring
without downloading a model or running real LLM inference:

```sh
./scripts/container-smoke-test.sh
```

This builds the `runtime` target with `DOWNLOAD_MODEL=0`, then checks:

- the compiled `/usr/local/bin/groovy-agent` and
  `/usr/local/bin/coreutils-mcp` binaries are present;
- `docker/entrypoint.sh` starts llama-server, waits for it to become
  ready, and forwards the container command to `groovy-agent` with the
  bundled MCP server configured;
- `llama-server` is started with the bundled coreutils MCP server
  registered (`--mcp-servers-json`), confined to the workspace, and not
  registered at all when `LLAMA_MCP_COREUTILS=0`;
- without a positional prompt, the entrypoint does not invoke
  `groovy-agent` and keeps llama-server serving until the container is
  stopped;
- the real `llama-server` binary spawns the real `coreutils-mcp` over
  stdio and discovers its tools (`MCP warmup: 'coreutils' discovered N
  tools`); this happens before the model is loaded, so a placeholder
  model file is enough and no inference ever runs;
- `docker run ... mcp` starts `coreutils-mcp --transport http` (and
  never `llama-server`), and completes a real `initialize` /
  `notifications/initialized` / `tools/list` / `tools/call` (`pwd`)
  exchange against it over MCP Streamable HTTP;
- the real `llama-server` binary, started against a committed placeholder
  GGUF (`scripts/testdata/chat-template-smoke-model.gguf` — a tiny,
  randomly initialized model whose architecture/tokenizer are valid but
  whose weights are never used to generate text), reports
  `chat_template_caps.supports_tools`/`supports_tool_calls: true` at
  `GET /props` with the bundled default `LLAMA_CHAT_TEMPLATE_FILE`.

The llama-server/groovy-agent forwarding checks replace those two
binaries inside the container with deterministic stub scripts (a
minimal HTTP server that answers `/health`, and a script that records
its argv), so no model, GPU, or CPU inference is required and no
llama-server port is ever published outside the container. The MCP
discovery check and the `mcp` mode check instead run the real
`coreutils-mcp` binary (still no model/GPU/CPU inference is involved);
the `mcp` mode check talks to it with `docker exec`, so its HTTP port is
never published outside the container either. Set
`CONTAINER_ENGINE=podman` to run it with Podman instead of Docker.

## Configuration / environment variables

Agent CLI flags (`cmd/agent`):

- `--llama-url` (default `http://127.0.0.1:8080`): base URL of the local
  `llama-server`; must be `http://` or `https://`.
- `--model` (default `local-phi-4-mini-instruct`): model name advertised
  to `llama-server`.
- `--mcp-command` (default `./bin/coreutils-mcp`): path to the coreutils
  MCP server executable.
- `--workspace` (default `.`): directory that bounds every filesystem
  operation performed by the MCP tools.
- remaining arguments are joined as the prompt.

`coreutils-mcp` CLI flags (`cmd/coreutils-mcp`):

- `--workspace` (default `.`): directory that bounds every filesystem
  operation.
- `--transport` (default `stdio`): `stdio` for a locally spawned
  MCP client, or `http` to serve the MCP Streamable HTTP transport.
- `--listen` (default `127.0.0.1:8765`, `--transport=http` only): bind
  address; defaults to loopback so it is not reachable from the network
  unless you deliberately rebind it.
- `--http-path` (default `/mcp`, `--transport=http` only): endpoint path.
- `--http-token` (default unset, `--transport=http` only): if set,
  requests must carry a matching bearer authorization header; if unset,
  the server logs a warning and accepts unauthenticated requests.

Container/`docker/entrypoint.sh` environment variables:

- `LLAMA_SERVER_HOST` (default `0.0.0.0`)
- `LLAMA_SERVER_PORT` (default `8080`)
- `LLAMA_MODEL_PATH` or `LLAMA_MODEL_FILE` (default filename
  `Phi-4-mini-instruct.Q8_0.gguf`, looked up under `/models`)
- `LLAMA_MODEL_NAME` (model alias passed to `llama-server --alias` and to
  `groovy-agent --model`; default `Phi-4-mini-instruct`)
- `LLAMA_CTX_SIZE` (default `8192`)
- `LLAMA_THREADS` (default: autodetected via `nproc`)
- `LLAMA_N_GPU_LAYERS` (default `0`)
- `LLAMA_STARTUP_TIMEOUT` (seconds, default `180`)
- `LLAMA_EXTRA_ARGS` (space-separated extra `llama-server` flags; avoid
  values containing spaces, and never source this from untrusted input)
- `LLAMA_CHAT_TEMPLATE_FILE` (default: a bundled tool-aware ChatML template,
  `docker/chat-templates/tool-use-chatml.jinja`, passed via
  `--chat-template-file`): the bundled Phi-4-mini GGUF's own embedded chat
  template does not render `tools`/`tool_calls`, so `--jinja` alone cannot
  produce structured tool calls (`GET /props` would report
  `chat_template_caps.supports_tools`/`supports_tool_calls: false`); this
  default template does, which was verified against the pinned llama.cpp
  runtime (build 10481, commit 25ae3a9b3) with `GET /props` reporting both
  as `true`. Ignored when `LLAMA_CHAT_TEMPLATE` is set.
- `LLAMA_CHAT_TEMPLATE` (default unset; passed via `--chat-template`):
  operator override taking a llama.cpp built-in template name (e.g.
  `chatml` — note this one does **not** support tools) or raw Jinja source,
  since `--jinja` is always set. Only set this to a template you have
  personally verified reports `chat_template_caps.supports_tools`/
  `supports_tool_calls: true` at `GET /props`; otherwise Web UI/API tool
  calls silently stop working and the model reverts to free-generating
  pseudo-shell text instead of invoking the registered MCP tools.
- `LLAMA_REPEAT_PENALTY` / `LLAMA_REPEAT_LAST_N` / `LLAMA_PREDICT_LIMIT`:
  sampling guardrails that curb small-model repetition loops
- `LLAMA_MCP_COREUTILS` (default `1`): register the bundled read-only
  coreutils MCP server with `llama-server` over stdio; set to `0` to
  start `llama-server` without any MCP tool set (this also implies no
  `--ui-mcp-proxy`, since it is only ever added alongside this
  registration)
- `LLAMA_MCP_UI_PROXY` (default `1`, only relevant when
  `LLAMA_MCP_COREUTILS` is enabled): start `llama-server` with
  `--ui-mcp-proxy`, letting the built-in Web UI's browser JavaScript reach
  further MCP servers registered from its own Settings panel; set to `0`
  to keep the bundled coreutils tools without this separate, Web-UI-only
  feature (see
  [Coreutils tools inside llama.cpp](#coreutils-tools-inside-llamacpp)
  above)
- `LLAMA_MCP_WORKSPACE` (default `${MCP_WORKSPACE:-${AGENT_OUTPUT_DIR:-/output}}`):
  directory the registered MCP tools are confined to (see
  [Coreutils tools inside llama.cpp](#coreutils-tools-inside-llamacpp)
  above)
- `AGENT_OUTPUT_DIR` (default `/output` in the container)
- `MCP_HTTP_HOST` (default `0.0.0.0`), `MCP_HTTP_PORT` (default `8765`),
  `MCP_HTTP_PATH` (default `/mcp`), `MCP_HTTP_TOKEN` (default unset), and
  `MCP_WORKSPACE` (default `${AGENT_OUTPUT_DIR:-/output}`): only used by
  the `mcp` container mode (see
  [Run the remote MCP server](#run-the-remote-mcp-server-no-llama-server)
  above).

## Quick smoke test

Once `llama-server` is reachable and the MCP binary is built (either via
Docker or locally, see above), verify the whole pipeline end to end:

```sh
./bin/groovy-agent \
  --llama-url http://127.0.0.1:8080 \
  --model Phi-4-mini-instruct \
  --mcp-command ./bin/coreutils-mcp \
  --workspace . \
  "what is the sha256sum of go.mod?"
```

A correct run prints diagnostics on stderr (workspace, model, discovered
tools, model rounds) and a single final answer line on stdout. If you
just want to validate the container packaging without any model, use
`./scripts/container-smoke-test.sh` instead (see above).

## MCP server standalone mode

`cmd/coreutils-mcp` can be pointed at by any MCP-compatible client as a
standalone bounded workspace/text MCP server, over either transport.

### stdio (local clients that spawn a child process)

This is the default (`--transport stdio`) and the most common way to
plug the tool set into a local MCP client:

```json
{
  "mcpServers": {
    "coreutils": {
      "command": "/absolute/path/to/coreutils-mcp",
      "args": ["--workspace", "/absolute/path/to/workspace"]
    }
  }
}
```

It speaks newline-delimited JSON-RPC over stdin/stdout; diagnostics go to
stderr only.

### Streamable HTTP (remote clients)

Run with `--transport http` to serve the same tool set over the
[MCP Streamable HTTP transport](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#streamable-http)
instead, for clients that connect over the network rather than spawning
a child process:

```sh
./bin/coreutils-mcp \
  --workspace /absolute/path/to/workspace \
  --transport http \
  --listen 127.0.0.1:8765 \
  --http-token change-me
```

Then point any Streamable-HTTP-capable MCP client at
`http://127.0.0.1:8765/mcp`, sending the configured bearer token in the
`Authorization` header (see the Docker section above for a client
configuration example and the security implications of publishing this
port beyond loopback). This is a plain `POST /mcp` JSON-RPC
request/response endpoint: the server never sends unsolicited messages,
so `GET`/`DELETE` (used for optional server-initiated streaming and
session termination) are answered with `405 Method Not Allowed`, and
batched JSON-RPC arrays are not supported.

Note that **llama.cpp's own web UI cannot connect to this endpoint**: it
has no way to be told about an MCP server at runtime, and the llama.cpp
MCP client in the pinned build only spawns MCP servers over stdio from
the server-side `--mcp-servers-json`/`--mcp-servers-config` flags. To use
the coreutils tool set from llama.cpp, rely on the automatic stdio
registration performed by the image
([Coreutils tools inside llama.cpp](#coreutils-tools-inside-llamacpp)).

## Removed / out of scope

This rebuild intentionally does not include (and will not add without a
new, explicit design): network-fetch tools, browser automation, GitHub or
other cloud-provider integrations, package-manager invocation, arbitrary
shell/command execution, unrestricted patch/application tools, multi-turn
session persistence, or a moderator/planner layer. The only integration
surface is the bounded MCP tool set described above, plus the local
`llama-server` HTTP API.
