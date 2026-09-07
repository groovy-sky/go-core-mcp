# PLAN.md

# Go MCP Coreutils Server — Implementation Plan

## 1. Purpose

Build a Go-based MCP server that exposes a controlled set of small, composable Unix-like utilities to MCP clients.

The server will use the existing `coreutils.Command` registry as its execution layer and provide an MCP-facing tool interface with strict validation, output limits, cancellation, authorization hooks, and auditability.

The system must **not** provide arbitrary shell execution, arbitrary binary execution, or unrestricted filesystem/network access.

---

## 2. Goals

### Primary goals

- Provide an MCP-compatible JSON-RPC server in Go.
- Reuse the existing `coreutils.Command` abstraction.
- Expose approved text-processing utilities to AI clients.
- Support structured tool inputs and predictable structured outputs.
- Enforce command allowlists and argument validation.
- Prevent unbounded memory, CPU, output, or input consumption.
- Support request cancellation through Go `context.Context`.
- Keep domain logic separate from MCP protocol handling.
- Make it straightforward to add per-command authorization and audit policies.

### Initial supported use cases

Examples:

- Sort text supplied through `stdin`.
- Count lines, words, or bytes.
- Extract fields from structured text.
- Transform text case or characters.
- Remove adjacent duplicate lines.
- Format text.
- Select leading or trailing lines.

### Non-goals

The first version will not support:

- Shell execution such as `sh -c`, `bash -c`, or command pipelines.
- Arbitrary executable invocation.
- Arbitrary filesystem paths.
- Arbitrary network access.
- Environment-variable inspection.
- Process creation.
- Privilege changes.
- File deletion, copying, moving, ownership changes, or permission changes.
- Stateful shell sessions.
- Long-running background jobs.

---

## 3. Technical Decisions

| Area | Decision |
|---|---|
| Language | Go |
| Protocol | MCP over JSON-RPC |
| Initial transport | `stdio` |
| Future remote transport | Streamable HTTP |
| MCP implementation | Official `modelcontextprotocol/go-sdk` |
| Execution model | Registered in-process Go commands only |
| Initial MCP interface | One constrained `coreutils_run` tool |
| Future MCP interface | Individual purpose-specific tools for common safe operations |
| Shell access | Prohibited |
| Command discovery | Internal registry; do not automatically expose every registered command |
| Output handling | Bounded stdout and stderr buffers |
| Input handling | Bounded UTF-8 text input |
| Timeout model | Caller-provided context plus server-side timeout policy |
| Authorization | Per-command policy hook |
| Auditing | Required for all invocations in production |

The MCP server should target the current MCP specification revision and use the official Go SDK rather than manually implementing JSON-RPC message handling. MCP uses JSON-RPC over standard transports including stdio and Streamable HTTP. 

---

## 4. Architecture

```text
MCP Host / Client
        │
        │ JSON-RPC over stdio
        ▼
┌────────────────────────────────────┐
│ MCP Server                          │
│                                    │
│ - Tool discovery                    │
│ - Tool invocation                   │
│ - Input schema validation           │
│ - Request context / cancellation    │
│ - MCP error mapping                 │
└────────────────┬───────────────────┘
                 │
                 ▼
┌────────────────────────────────────┐
│ Coreutils MCP Adapter               │
│                                    │
│ - Command allowlist                 │
│ - Argument validation               │
│ - Input-size validation             │
│ - Output-size limits                │
│ - Authorization hook                │
│ - Audit hook                        │
│ - Result formatting                 │
└────────────────┬───────────────────┘
                 │
                 ▼
┌────────────────────────────────────┐
│ coreutils Package                   │
│                                    │
│ - Command registry                  │
│ - Named command lookup              │
│ - In-process command execution      │
└────────────────┬───────────────────┘
                 │
                 ▼
┌────────────────────────────────────┐
│ Registered Command Implementations  │
│                                    │
│ sort / uniq / wc / tr / head / ...  │
└────────────────────────────────────┘
```

---

## 5. Repository Layout

```text
.
├── cmd/
│   └── mcp-coreutils/
│       └── main.go
├── internal/
│   ├── audit/
│   │   ├── audit.go
│   │   └── logger.go
│   ├── auth/
│   │   ├── authorizer.go
│   │   └── policy.go
│   ├── coreutilsmcp/
│   │   ├── executor.go
│   │   ├── limits.go
│   │   ├── tool.go
│   │   └── validator.go
│   ├── config/
│   │   └── config.go
│   └── server/
│       └── server.go
├── pkg/
│   └── coreutils/
│       ├── command.go
│       ├── sort.go
│       ├── uniq.go
│       ├── wc.go
│       └── ...
├── tests/
│   ├── integration/
│   └── fixtures/
├── go.mod
├── go.sum
├── README.md
└── PLAN.md
```

---

## 6. Coreutils Package Changes

The current `Command` type is a good minimal execution abstraction:

```go
type Command struct {
	Name        string
	Description string
	Run         func(context.Context, []string, io.Reader, io.Writer, io.Writer) error
}
```

Extend it with metadata needed for safe MCP exposure.

```go
type Command struct {
	Name        string
	Description string

	// ExposeToMCP determines whether this command may be used by MCP clients.
	ExposeToMCP bool

	// ReadOnly indicates whether execution has no external side effects.
	ReadOnly bool

	// ValidateArgs validates command-specific arguments before execution.
	ValidateArgs func(args []string) error

	// Run executes the command.
	Run func(
		ctx context.Context,
		args []string,
		stdin io.Reader,
		stdout io.Writer,
		stderr io.Writer,
	) error
}
```

### Rules

1. `Name` must be unique.
2. `Name` must be stable after release.
3. `ExposeToMCP` defaults to `false`.
4. Every MCP-exposed command must provide `ValidateArgs`.
5. MCP-exposed commands must honor context cancellation.
6. Commands must not invoke a shell.
7. Commands must not access files, network resources, environment variables, or subprocesses unless explicitly approved through a separate design review.

---

## 7. Initial Safe Command Allowlist

Initial commands should operate only on supplied stdin and should not accept filesystem paths.

| Command | Initial status | Allowed arguments |
|---|---:|---|
| `sort` | Enabled | `-r`, `--reverse`, `-n`, `--numeric-sort` |
| `uniq` | Enabled | `-c`, `--count`, `-d`, `--repeated` |
| `wc` | Enabled | `-l`, `-w`, `-c` |
| `tr` | Enabled | Restricted character-set arguments |
| `head` | Enabled | `-n <count>` only |
| `tail` | Enabled | `-n <count>` only |
| `cut` | Enabled | Explicit field or character ranges only |
| `fmt` | Enabled | Width-related options only |
| `cat` | Optional | No arguments; stdin only |

The following classes of utilities must remain disabled in the generic tool:

```text
rm, mv, cp, chmod, chown
sh, bash, zsh, cmd, powershell
curl, wget, nc, ssh
find with path traversal
grep with arbitrary file paths
env, printenv
ps, kill, pkill
sudo, su
tar, zip, unzip
sed with file-writing behavior
```

---

## 8. MCP Tool Design

## 8.1 Initial Tool: `coreutils_run`

The initial version exposes a single generic tool for approved commands.

### Input

```json
{
  "command": "sort",
  "args": ["--reverse"],
  "stdin": "pear\napple\norange\n"
}
```

### Input schema

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "command": {
      "type": "string",
      "description": "Name of an approved core utility."
    },
    "args": {
      "type": "array",
      "description": "Validated utility arguments. Shell syntax is not supported.",
      "items": {
        "type": "string"
      }
    },
    "stdin": {
      "type": "string",
      "description": "Optional UTF-8 text provided to the command through standard input."
    }
  },
  "required": ["command"]
}
```

### Output

```json
{
  "command": "sort",
  "stdout": "pear\norange\napple\n",
  "stderr": "",
  "truncated": false
}
```

### Error examples

```text
command "rm" is not permitted
invalid arguments: unsupported sort argument "--output"
stdin exceeds maximum allowed size
command output exceeds maximum allowed size
request deadline exceeded
```

---

## 8.2 Future Tool Model

After command behavior and policies stabilize, replace or supplement `coreutils_run` with dedicated tools:

```text
text_sort
text_count
text_unique_lines
text_transform
text_take_first_lines
text_take_last_lines
text_extract_fields
```

Benefits:

- Better model-facing descriptions.
- Smaller and more precise schemas.
- Less argument ambiguity.
- Easier per-tool authorization.
- Safer defaults.
- More useful telemetry.
- Better client UX.

Example future tool:

```json
{
  "name": "text_sort",
  "description": "Sort newline-delimited text.",
  "inputSchema": {
    "type": "object",
    "additionalProperties": false,
    "properties": {
      "text": {
        "type": "string"
      },
      "reverse": {
        "type": "boolean",
        "default": false
      },
      "numeric": {
        "type": "boolean",
        "default": false
      }
    },
    "required": ["text"]
  }
}
```

---

## 9. Execution Adapter

Create an adapter between MCP input and `coreutils.Run`.

```go
type RunInput struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Stdin   string   `json:"stdin"`
}

type RunOutput struct {
	Command   string `json:"command"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr,omitempty"`
	Truncated bool   `json:"truncated"`
}

type Executor struct {
	AllowedCommands map[string]struct{}

	MaxStdinBytes  int
	MaxStdoutBytes int
	MaxStderrBytes int

	Authorizer Authorizer
	AuditSink  AuditSink
}
```

Execution sequence:

1. Validate request structure.
2. Normalize and validate command name.
3. Check command allowlist.
4. Retrieve command from `coreutils.Commands()`.
5. Verify `ExposeToMCP`.
6. Run command-specific `ValidateArgs`.
7. Enforce input-size limit.
8. Create bounded stdout and stderr writers.
9. Invoke authorization policy.
10. Execute command with caller context.
11. Record audit event.
12. Return structured output or safe error.

---

## 10. Resource Limits

Initial defaults:

| Limit | Default |
|---|---:|
| Maximum stdin | 64 KiB |
| Maximum stdout | 256 KiB |
| Maximum stderr | 32 KiB |
| Maximum argument count | 32 |
| Maximum argument length | 4 KiB |
| Default execution timeout | 5 seconds |
| Maximum execution timeout | 30 seconds |
| Maximum concurrent executions | 16 |
| Per-client request rate | Configurable |
| Maximum line length | Command-specific |

Limits must be configurable through environment variables or configuration files.

Example:

```text
COREUTILS_MAX_STDIN_BYTES=65536
COREUTILS_MAX_STDOUT_BYTES=262144
COREUTILS_MAX_STDERR_BYTES=32768
COREUTILS_EXECUTION_TIMEOUT=5s
COREUTILS_MAX_CONCURRENCY=16
```

---

## 11. Cancellation and Timeouts

Every command must receive the request context.

```go
err := coreutils.Run(
	ctx,
	input.Command,
	input.Args,
	stdin,
	stdout,
	stderr,
)
```

Requirements:

- Commands must periodically check `ctx.Done()` during iterative work.
- Blocking operations must use context-aware APIs where possible.
- The server must return `context.DeadlineExceeded` when the execution deadline expires.
- A cancelled request must not continue consuming resources indefinitely.
- Future commands that spawn subprocesses must explicitly terminate child processes on cancellation.

MCP defines cancellation behavior, but application code must still propagate cancellation to its own work. 

---

## 12. Authorization Design

The initial local `stdio` version may use a permissive development authorizer, but the interface must exist from the start.

```go
type Principal struct {
	ID       string
	TenantID string
	Roles    []string
}

type Authorizer interface {
	Authorize(
		ctx context.Context,
		principal Principal,
		command coreutils.Command,
		args []string,
	) error
}
```

Default policy:

| Command class | Policy |
|---|---|
| Safe read-only text transformations | Allow |
| Commands exposing filesystem or environment data | Deny |
| Commands with external side effects | Deny |
| Commands requiring elevated privileges | Deny |
| Unknown commands | Deny |

For future Streamable HTTP deployment, authentication and authorization must be tied to the authenticated client identity, scopes, and tenant boundaries. 

---

## 13. Audit Logging

Every production invocation should produce an audit event.

```go
type AuditEvent struct {
	Timestamp   time.Time
	RequestID   string
	PrincipalID string
	Command     string
	Args        []string

	Allowed      bool
	Success      bool
	DurationMS   int64
	StdoutBytes  int
	StderrBytes  int
	ErrorCode    string
	WasTruncated bool
}
```

Do not log:

- Raw stdin by default.
- Raw stdout by default.
- Credentials, tokens, cookies, or API keys.
- Sensitive personal information.
- Internal stack traces.

Use structured logs written to `stderr`; never write logs to `stdout` in stdio mode because stdout carries MCP JSON-RPC traffic.

---

## 14. Error Handling

Errors must be categorized.

| Category | Example | Client behavior |
|---|---|---|
| Invalid input | Missing command | Correct request |
| Unknown command | `command "foo" is unknown` | Choose supported command |
| Forbidden command | `command "rm" is not permitted` | Deny |
| Invalid arguments | Unsupported option | Correct arguments |
| Resource limit | Input/output limit exceeded | Reduce request |
| Timeout | Deadline exceeded | Retry with smaller work |
| Cancellation | Request cancelled | Stop |
| Internal error | Unexpected implementation failure | Return safe generic message |

Rules:

- Do not expose Go stack traces.
- Do not expose filesystem paths, environment variables, or internal service addresses.
- Preserve error categories in metrics and audit logs.
- Return stderr only when it is safe and useful.

---

## 15. Testing Plan

## 15.1 Unit tests

Test:

- Command registration.
- Duplicate command prevention.
- Command ordering.
- Unknown command behavior.
- Argument validation.
- Allowlist enforcement.
- `ExposeToMCP` enforcement.
- Input-size limits.
- Output-size limits.
- Timeout behavior.
- Context cancellation.
- Error mapping.
- Audit event creation.

## 15.2 Command tests

Each command must test:

- Normal input.
- Empty input.
- Unicode input.
- Large input near configured limit.
- Invalid flags.
- Invalid flag combinations.
- Cancellation.
- Output limit behavior.
- Deterministic output.

## 15.3 MCP integration tests

Validate:

- Server starts over stdio.
- Tool discovery returns expected tools.
- Valid `coreutils_run` invocation succeeds.
- Invalid schemas are rejected.
- Unknown commands are rejected.
- Disabled commands are rejected.
- Logs do not corrupt JSON-RPC output.
- Concurrent calls remain isolated.
- Cancellation stops execution.
- Server shutdown is graceful.

## 15.4 Security tests

Test attempts to:

- Invoke shell commands.
- Use command separators such as `;`, `&&`, `|`, `$()`, and backticks.
- Pass path traversal values.
- Produce excessive output.
- Send excessive stdin.
- Exhaust concurrent execution capacity.
- Bypass command validation.
- Invoke non-exposed registered commands.
- Trigger panics through malformed input.

---

## 16. Delivery Milestones

## Milestone 1 — Server Skeleton

Deliverables:

- Go module.
- MCP server process.
- stdio transport.
- Health or echo tool.
- Structured stderr logging.
- Graceful shutdown.

Success criteria:

- Compatible MCP client can discover and call a test tool.

## Milestone 2 — Coreutils Adapter

Deliverables:

- `RunInput` and `RunOutput`.
- Command allowlist.
- Command lookup.
- Bounded input/output readers and writers.
- Generic `coreutils_run` MCP tool.

Success criteria:

- `sort`, `uniq`, and `wc` execute safely against stdin-only input.

## Milestone 3 — Safety Controls

Deliverables:

- Per-command validators.
- Timeout policy.
- Context cancellation tests.
- Concurrency limiter.
- Audit sink.
- Authorization interface.

Success criteria:

- Disallowed commands, dangerous arguments, oversized input, and oversized output are rejected.

## Milestone 4 — Production Readiness

Deliverables:

- Metrics.
- Structured audit logs.
- Integration test suite.
- Documentation.
- Configuration reference.
- Container build.
- Release process.

Success criteria:

- Server is deployable, observable, and repeatably testable.

## Milestone 5 — Dedicated Tools

Deliverables:

- `text_sort`
- `text_count`
- `text_unique_lines`
- `text_transform`
- `text_take_first_lines`
- `text_take_last_lines`

Success criteria:

- Common operations no longer require generic argument arrays.

---

## 17. Acceptance Criteria

The first production-ready version is complete when:

- [ ] The server runs through stdio without writing non-protocol data to stdout.
- [ ] The MCP client can discover and invoke the available tool.
- [ ] Only explicitly approved commands can run.
- [ ] Every exposed command validates its arguments.
- [ ] No shell or subprocess execution is available.
- [ ] Inputs, outputs, execution time, and concurrency are bounded.
- [ ] Commands observe `context.Context` cancellation.
- [ ] Errors are safe and categorized.
- [ ] Every invocation generates an audit event.
- [ ] Unit, integration, and adversarial tests pass.
- [ ] The default configuration exposes only read-only, stdin-only utilities.
- [ ] Documentation explains enabled commands, limits, and safety guarantees.

