// Package mcpserver implements the coreutils MCP server. It speaks JSON-RPC
// 2.0 over stdio, exposes a bounded workspace/text tool set, and enforces the
// workspace boundary and the resource limits from PLAN.md.
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/jsonschema"
	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

// Limits are the bounded execution defaults from PLAN.md.
type Limits struct {
	MaxResultBytes   int
	MaxFileReadBytes int
	MaxGrepMatches   int
	MaxLineBytes     int
	MaxDuration      time.Duration
}

// DefaultLimits returns the PLAN.md defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxResultBytes:   16 << 10,
		MaxFileReadBytes: 12 << 10,
		MaxGrepMatches:   20,
		MaxLineBytes:     2 << 10,
		MaxDuration:      10 * time.Second,
	}
}

// toolError carries an error category back to the client.
type toolError struct {
	Category string
	Message  string
}

func (e *toolError) Error() string { return e.Message }

func fail(category, format string, args ...any) *toolError {
	return &toolError{Category: category, Message: fmt.Sprintf(format, args...)}
}

// handler executes a validated tool invocation.
type handler func(context.Context, *Server, map[string]any) (payload, error)

// payload is the structured result returned to the model.
type payload struct {
	Output    string         `json:"output"`
	Truncated bool           `json:"truncated"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Result    map[string]any `json:"-"`
}

type tool struct {
	name        string
	description string
	schema      map[string]any
	run         handler
}

// Server serves the coreutils tool set for a single workspace.
type Server struct {
	workspace string
	limits    Limits
	logger    *log.Logger
	tools     map[string]tool

	writeMutex  sync.Mutex
	initialized bool
}

// New creates a server rooted at the canonicalized workspace directory.
// Diagnostics are written to logger, which must never be stdout.
func New(workspace string, limits Limits, logger *log.Logger) (*Server, error) {
	root, err := canonicalWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	server := &Server{workspace: root, limits: limits, logger: logger, tools: map[string]tool{}}
	for _, definition := range definitions() {
		if _, duplicate := server.tools[definition.name]; duplicate {
			return nil, fmt.Errorf("duplicate tool definition %q", definition.name)
		}
		server.tools[definition.name] = definition
	}
	return server, nil
}

// Workspace returns the canonical workspace root.
func (s *Server) Workspace() string { return s.workspace }

// ToolNames returns the exposed tool names in a stable order.
func (s *Server) ToolNames() []string {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Serve reads newline delimited JSON-RPC messages from input and writes
// responses to output. It returns when the input stream closes, which is how
// the server terminates once its parent connection is gone.
func (s *Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		request := mcpproto.Message{}
		if err := json.Unmarshal(line, &request); err != nil {
			s.respondError(output, nil, mcpproto.CodeParseError, "malformed JSON-RPC message")
			continue
		}
		s.dispatch(ctx, output, request)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read MCP request: %w", err)
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, output io.Writer, request mcpproto.Message) {
	isNotification := len(request.ID) == 0
	switch request.Method {
	case "initialize":
		s.initialized = true
		result := mcpproto.InitializeResult{
			ProtocolVersion: mcpproto.Version,
			Capabilities:    mcpproto.ServerCapabilities{Tools: &mcpproto.ToolsCapability{}},
			ServerInfo:      mcpproto.Implementation{Name: "coreutils-mcp", Version: "1.0.0"},
		}
		s.respondResult(output, request.ID, result)
	case "notifications/initialized", "notifications/cancelled":
		// Notifications carry no response.
	case "ping":
		s.respondResult(output, request.ID, map[string]any{})
	case "tools/list":
		s.respondResult(output, request.ID, s.listTools())
	case "tools/call":
		if isNotification {
			return
		}
		s.respondResult(output, request.ID, s.callTool(ctx, request.Params))
	default:
		if isNotification {
			return
		}
		s.respondError(output, request.ID, mcpproto.CodeMethodNotFound, "unsupported method")
	}
}

func (s *Server) listTools() mcpproto.ListToolsResult {
	names := s.ToolNames()
	tools := make([]mcpproto.Tool, 0, len(names))
	for _, name := range names {
		definition := s.tools[name]
		schema, err := json.Marshal(definition.schema)
		if err != nil {
			continue
		}
		tools = append(tools, mcpproto.Tool{
			Name:        definition.name,
			Description: definition.description,
			InputSchema: schema,
		})
	}
	return mcpproto.ListToolsResult{Tools: tools}
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) mcpproto.CallToolResult {
	params := mcpproto.CallToolParams{}
	if err := json.Unmarshal(raw, &params); err != nil {
		return errorResult(mcpproto.ErrorInvalidArguments, "tool call parameters are not a JSON object")
	}
	definition, ok := s.tools[params.Name]
	if !ok {
		s.logf("denied unknown tool request")
		return errorResult(mcpproto.ErrorUnknownTool, "tool is not available")
	}
	arguments, err := jsonschema.ValidateRaw(definition.schema, params.Arguments)
	if err != nil {
		return errorResult(mcpproto.ErrorInvalidArguments, err.Error())
	}

	callCtx, cancel := context.WithTimeout(ctx, s.limits.MaxDuration)
	defer cancel()

	started := time.Now()
	result, err := definition.run(callCtx, s, arguments)
	duration := time.Since(started)
	if err != nil {
		category := mcpproto.ErrorToolError
		message := "tool execution failed"
		var typed *toolError
		if errors.As(err, &typed) {
			category, message = typed.Category, typed.Message
		}
		if errors.Is(err, context.DeadlineExceeded) || callCtx.Err() != nil {
			category, message = mcpproto.ErrorTimeout, "tool exceeded its time budget"
		}
		s.logf("tool=%s duration=%s error=%s", definition.name, duration.Round(time.Millisecond), category)
		return errorResult(category, message)
	}

	output, truncated := clampResult(result.Output, s.limits.MaxResultBytes)
	result.Output = output
	result.Truncated = result.Truncated || truncated
	s.logf("tool=%s duration=%s bytes=%d truncated=%t", definition.name, duration.Round(time.Millisecond), len(result.Output), result.Truncated)

	body := result.Result
	if body == nil {
		body = map[string]any{"success": true, "output": result.Output, "truncated": result.Truncated}
	}
	if len(result.Metadata) > 0 {
		body["metadata"] = result.Metadata
	}
	return mcpproto.CallToolResult{Content: []mcpproto.Content{{Type: "text", Text: encode(body)}}}
}

func clampResult(output string, limit int) (string, bool) {
	if len(output) <= limit {
		return output, false
	}
	return output[:limit], true
}

func errorResult(category, message string) mcpproto.CallToolResult {
	body := map[string]any{"success": false, "error": category, "message": message}
	return mcpproto.CallToolResult{
		Content: []mcpproto.Content{{Type: "text", Text: encode(body)}},
		IsError: true,
	}
}

func encode(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"success":false,"error":"tool_error","message":"result could not be encoded"}`
	}
	return string(encoded)
}

func (s *Server) respondResult(output io.Writer, id json.RawMessage, result any) {
	if len(id) == 0 {
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		s.respondError(output, id, mcpproto.CodeInternalError, "result could not be encoded")
		return
	}
	s.write(output, mcpproto.Message{JSONRPC: "2.0", ID: id, Result: encoded})
}

func (s *Server) respondError(output io.Writer, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	s.write(output, mcpproto.Message{JSONRPC: "2.0", ID: id, Error: &mcpproto.Error{Code: code, Message: message}})
}

func (s *Server) write(output io.Writer, message mcpproto.Message) {
	encoded, err := json.Marshal(message)
	if err != nil {
		s.logf("failed to encode response")
		return
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	if _, err := output.Write(append(encoded, '\n')); err != nil {
		s.logf("failed to write response")
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Printf(format, args...)
}
