package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/jsonschema"
	"github.com/groovy-sky/groovy-agent/internal/llm"
	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

// requestedLimits caps the bounded arguments the model may ask for. The model
// can never raise a safety limit.
var requestedLimits = map[string]int{
	"max_bytes":     12 << 10,
	"lines":         200,
	"max_matches":   200,
	"max_results":   200,
	"context_lines": 5,
}

// validationError describes a rejected tool call.
type validationError struct {
	category string
	message  string
}

func (e *validationError) Error() string { return e.message }

func reject(category, format string, args ...any) *validationError {
	return &validationError{category: category, message: fmt.Sprintf(format, args...)}
}

// runToolCall validates and executes one model requested tool call. The
// returned error is fatal: it means the MCP transport is unusable.
func (s *Session) runToolCall(ctx context.Context, exposed map[string]mcpproto.Tool, call llm.ToolCall, round, index int) (llm.Message, error) {
	id := strings.TrimSpace(call.ID)
	if id == "" {
		s.logger.Printf("rejected tool call without an ID")
		id = fmt.Sprintf("call_%d_%d", round, index)
		return toolMessage(id, call.Function.Name, errorPayload(mcpproto.ErrorInvalidArguments, "tool call is missing an id")), nil
	}

	arguments, tool, failure := s.validateCall(exposed, call)
	if failure != nil {
		s.logger.Printf("tool=%s rejected error=%s", call.Function.Name, failure.category)
		return toolMessage(id, call.Function.Name, errorPayload(failure.category, failure.message)), nil
	}

	s.toolCalls++
	started := time.Now()
	result, err := s.mcp.CallTool(ctx, tool.Name, arguments)
	duration := time.Since(started)
	if err != nil {
		if ctx.Err() != nil {
			return llm.Message{}, ctx.Err()
		}
		return llm.Message{}, fmt.Errorf("MCP session is unusable: %w", err)
	}

	text := result.Text()
	if len(text) > MaxResultBytes {
		s.logger.Printf("tool=%s duration=%s error=%s", tool.Name, duration.Round(time.Millisecond), mcpproto.ErrorResultTooLarge)
		return toolMessage(id, tool.Name, errorPayload(mcpproto.ErrorResultTooLarge, "tool result exceeded the result limit")), nil
	}
	s.logger.Printf("tool=%s args=%s duration=%s bytes=%d", tool.Name, boundedArguments(arguments), duration.Round(time.Millisecond), len(text))
	return toolMessage(id, tool.Name, normalizeResult(text, result.IsError)), nil
}

// validateCall enforces every model-output check required by PLAN.md.
func (s *Session) validateCall(exposed map[string]mcpproto.Tool, call llm.ToolCall) (json.RawMessage, mcpproto.Tool, *validationError) {
	if call.Type != "" && call.Type != "function" {
		return nil, mcpproto.Tool{}, reject(mcpproto.ErrorInvalidArguments, "unsupported tool call type")
	}
	if s.toolCalls >= MaxTotalToolCalls {
		return nil, mcpproto.Tool{}, reject(mcpproto.ErrorToolError, "tool call budget is exhausted")
	}
	tool, ok := exposed[call.Function.Name]
	if !ok {
		return nil, mcpproto.Tool{}, reject(mcpproto.ErrorUnknownTool, "tool is not available in this request")
	}

	raw := json.RawMessage(strings.TrimSpace(call.Function.Arguments))
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if !bytes.HasPrefix(raw, []byte("{")) {
		return nil, mcpproto.Tool{}, reject(mcpproto.ErrorInvalidArguments, "arguments must be a JSON object")
	}
	schema := map[string]any{}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		return nil, mcpproto.Tool{}, reject(mcpproto.ErrorToolError, "tool schema is unusable")
	}
	arguments, err := jsonschema.ValidateRaw(schema, raw)
	if err != nil {
		return nil, mcpproto.Tool{}, reject(mcpproto.ErrorInvalidArguments, "%s", err.Error())
	}
	for key, value := range arguments {
		if limit, bounded := requestedLimits[key]; bounded {
			requested, ok := jsonschema.Number(value)
			if !ok || requested > limit {
				return nil, mcpproto.Tool{}, reject(mcpproto.ErrorInvalidArguments, "%q exceeds the configured limit", key)
			}
		}
		if key != "path" && key != "root" {
			continue
		}
		path, ok := value.(string)
		if !ok {
			return nil, mcpproto.Tool{}, reject(mcpproto.ErrorInvalidArguments, "\"path\" must be a string")
		}
		if tool.Name == "basename" || tool.Name == "dirname" {
			// These tools operate on path text only and touch no files.
			continue
		}
		if err := validateWorkspacePath(path); err != nil {
			return nil, mcpproto.Tool{}, err
		}
	}
	return raw, tool, nil
}

// validateWorkspacePath rejects absolute paths and traversal before the call
// reaches the MCP server, which enforces the boundary again.
func validateWorkspacePath(path string) *validationError {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return reject(mcpproto.ErrorInvalidArguments, "\"path\" must not be empty")
	}
	if strings.ContainsRune(trimmed, '\x00') {
		return reject(mcpproto.ErrorInvalidArguments, "\"path\" contains an invalid character")
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") {
		return reject(mcpproto.ErrorWorkspaceViolation, "absolute paths are not allowed; use workspace-relative paths")
	}
	cleaned := filepath.Clean(trimmed)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return reject(mcpproto.ErrorWorkspaceViolation, "path escapes the workspace")
	}
	return nil
}

// normalizeResult keeps the compact structured payload produced by the MCP
// server and wraps anything unexpected.
func normalizeResult(text string, isError bool) string {
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(text), &decoded); err == nil {
		if _, ok := decoded["success"]; ok {
			return text
		}
	}
	if isError {
		return errorPayload(mcpproto.ErrorToolError, "tool reported an error")
	}
	body := map[string]any{"success": true, "output": text, "truncated": false}
	return encodePayload(body)
}

func errorPayload(category, message string) string {
	return encodePayload(map[string]any{"success": false, "error": category, "message": message})
}

func encodePayload(body map[string]any) string {
	encoded, err := json.Marshal(body)
	if err != nil {
		return `{"success":false,"error":"tool_error","message":"result could not be encoded"}`
	}
	return string(encoded)
}

func toolMessage(id, name, content string) llm.Message {
	return llm.Message{Role: "tool", ToolCallID: id, Name: name, Content: content}
}

// boundedArguments renders arguments for logging without unbounded content.
func boundedArguments(raw json.RawMessage) string {
	const limit = 200
	text := string(raw)
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}
