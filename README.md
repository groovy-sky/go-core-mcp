# groovy-agent

A minimal, reliable Go agent that answers a single prompt using a local
[`llama.cpp`](https://github.com/ggml-org/llama.cpp) `llama-server` and a
bounded set of **read-only coreutils tools** exposed over the
[Model Context Protocol (MCP)](https://modelcontextprotocol.io/).

The design intentionally has no non-coreutils integrations: no network
fetch, no browser, no GitHub/cloud APIs, no package management, no shell
string execution, and no file-write tools. See
[Architecture](#architecture) and [Security boundaries](#security-boundaries)
below for the exact guarantees.

## Model

The bundled/default model is
[`Phi-4-mini-instruct.Q8_0.gguf`](https://huggingface.co/unsloth/Phi-4-mini-instruct-GGUF/blob/main/Phi-4-mini-instruct.Q8_0.gguf)
from `unsloth/Phi-4-mini-instruct-GGUF`, served locally by `llama-server`.
The GGUF file is **never committed to git**; it is downloaded at
build/run time (see below).

The repository currently publishes two variants from the same Dockerfile:
`ghcr.io/<owner>/groovy-agent:phi4-mini` and
`ghcr.io/<owner>/groovy-agent:qwen3-8b`. Both are built through GitHub
Actions by setting `DOWNLOAD_MODEL=1` and overriding the model
URL/filename/name as needed.

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
mode that serves the same read-only tool set over the network for any
remote MCP-compatible client, instead of being spawned as the agent's
stdio child process:

```text
Remote MCP client ── MCP Streamable HTTP ──► coreutils MCP server (cmd/coreutils-mcp --transport http)
```

This does **not** go through `llama-server` or the Go agent at all; it is
a standalone deployment of the coreutils MCP server for clients that speak
MCP natively (see [MCP server standalone mode](#mcp-server-standalone-mode)
below).
```

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

There is no shell execution, no free-form command string, no write/mutate
tools, and no long-running session state. Each run answers exactly one
prompt and exits.

## Supported MCP tools

The coreutils MCP server (`internal/mcpserver`) exposes exactly these
read-only tools, each with a strict JSON-schema argument shape
(`additionalProperties: false`, bounded string/array lengths):

| Tool        | Purpose                                             |
|-------------|------------------------------------------------------|
| `pwd`       | Print the logical workspace path                      |
| `date`      | Print the current date/time (optionally UTC)          |
| `cat`       | Read a bounded prefix of a workspace text file         |
| `head`      | Read the first N lines of a workspace file             |
| `tail`      | Read the last N lines of a workspace file               |
| `wc`        | Count lines/words/bytes of a file or supplied text      |
| `grep`      | Search a workspace file for a pattern (bounded matches)  |
| `sha256sum` | Compute the SHA-256 digest of a workspace file          |
| `basename`  | Strip directory/suffix from a path (string op, no I/O)  |
| `dirname`   | Strip the last path component (string op, no I/O)       |
| `base64`    | Encode/decode base64 text                                |
| `cut`       | Select delimiter-separated fields from text              |
| `paste`     | Merge several texts line by line                         |
| `sort`      | Sort lines of supplied text                              |
| `tr`        | Translate/delete characters in text                      |
| `uniq`      | Remove adjacent duplicate lines                          |

Only a subset of the above (`AllowedTools` in `internal/agent/agent.go`) is
ever exposed to the model, and only a profile-selected slice (≤6 tools) is
sent per request. Write-capable coreutils
(`cp`, `link`, `mkdir`, `rmdir`, `tee`, `touch`, `unlink`) are **not
implemented** by the server at all (`mcpserver.WriteCapableTools` documents
this policy so it is explicit and tested).

## Security boundaries

- **No shell execution.** Every tool is a Go function operating on parsed,
  schema-validated arguments; there is no `sh -c`, `exec.Command` with a
  shell, or string concatenation into a command line anywhere in the tool
  dispatch path.
- **Workspace confinement.** All file tools resolve paths through
  `internal/mcpserver/workspace.go`, which:
  - rejects empty paths, NUL bytes, and absolute paths;
  - rejects `..` traversal before and after `filepath.Clean`;
  - resolves symlinks and re-checks the result stays inside the canonical,
    symlink-resolved workspace root (blocking symlink escapes);
  - reports safe, generic errors (no host path leakage).
- **Bounded I/O.** File reads, hash inputs, grep matches, and tool results
  all have fixed byte/line caps (`internal/mcpserver/server.go:
  DefaultLimits`), so a single tool call cannot exhaust memory or the
  model's context.
- **Allowlist, not trust-the-server.** The agent filters MCP `tools/list`
  results against its own hard-coded `AllowedTools`, so even if the MCP
  server were modified or replaced, the agent will not send unexpected
  tool schemas to the model or execute unexpected tool calls.
- **Local-only inference.** `llama-server` is only reachable via
  `--llama-url`, which must be an `http://` or `https://` URL; the
  container/entrypoint wires this to `127.0.0.1` by default and never
  forwards it to an external API.
- **No mutation tools.** There is no `write_file`, `apply_patch`,
  `exec_command`, or arbitrary command runner in this design.

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
  read-only coreutils tool set over the MCP Streamable HTTP transport for
  any remote MCP-compatible client (see [Run the remote MCP server](#run-the-remote-mcp-server-no-llama-server)
  below).

### Build with the model baked into the image

```sh
HF_TOKEN=... DOWNLOAD_MODEL_AT_BUILD=1 ./scripts/package-image.sh
```

(`HF_TOKEN` is optional and only needed for gated/rate-limited downloads.)
`MODEL_URL`, `MODEL_FILENAME`, and `MODEL_NAME` can also be overridden when
building custom variants. The script produces a local image
(`groovy-agent:local` by default) and saves a tarball to
`output/groovy-agent.tar`.

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
`Phi-4-mini-instruct`). Note that `llama-server` allows all CORS origins
and uses no API key, so only expose it on trusted networks.

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
not** point llama.cpp's built-in web UI at this endpoint: that UI is only
an OpenAI-compatible chat client and has no concept of MCP servers, so it
will never show or connect to `coreutils-mcp` no matter which port you
give it. Instead, configure an MCP-capable client to connect directly to
the Streamable HTTP endpoint, for example:

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
- without a positional prompt, the entrypoint does not invoke
  `groovy-agent` and keeps llama-server serving until the container is
  stopped;
- `docker run ... mcp` starts `coreutils-mcp --transport http` (and
  never `llama-server`), and completes a real `initialize` /
  `notifications/initialized` / `tools/list` / `tools/call` (`pwd`)
  exchange against it over MCP Streamable HTTP.

The llama-server/groovy-agent forwarding checks replace those two
binaries inside the container with deterministic stub scripts (a
minimal HTTP server that answers `/health`, and a script that records
its argv), so no model, GPU, or CPU inference is required and no
llama-server port is ever published outside the container. The `mcp`
mode check instead runs the real `coreutils-mcp` binary (still no
model/GPU/CPU inference is involved) and talks to it with `docker exec`,
so its HTTP port is never published outside the container either. Set
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
- `LLAMA_REPEAT_PENALTY` / `LLAMA_REPEAT_LAST_N` / `LLAMA_PREDICT_LIMIT`:
  sampling guardrails that curb small-model repetition loops
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
standalone read-only coreutils server, over either transport.

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

Note that **llama.cpp's own web UI cannot connect to this endpoint or
any MCP server**: it is a chat UI for `llama-server`'s
OpenAI-compatible API and has no MCP client support, so it will never
list `coreutils-mcp` regardless of how this server is deployed.

## Removed / out of scope

This rebuild intentionally does not include (and will not add without a
new, explicit design): network-fetch tools, browser automation, GitHub or
other cloud-provider integrations, package-manager invocation, arbitrary
shell/command execution, file-write/patch tools, multi-turn session
persistence, or a moderator/planner layer. The only integration surface
is the coreutils MCP tool set described above, plus the local
`llama-server` HTTP API.
