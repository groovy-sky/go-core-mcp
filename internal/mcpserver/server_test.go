package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

func newTestServer(t *testing.T, workspace string) *Server {
	t.Helper()
	server, err := New(workspace, DefaultLimits(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return server
}

func call(t *testing.T, server *Server, arguments map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(mcpproto.CallToolParams{Name: "coreutils_run", Arguments: mustJSON(t, arguments)})
	if err != nil {
		t.Fatalf("encode params: %v", err)
	}
	result := server.callTool(context.Background(), encoded)
	body := map[string]any{}
	if err := json.Unmarshal([]byte(result.Text()), &body); err != nil {
		t.Fatalf("tool result is not JSON: %v", err)
	}
	return body
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}

func expectError(t *testing.T, body map[string]any, category string) {
	t.Helper()
	if body["success"] != false || body["error"] != category {
		t.Fatalf("expected %q failure, got %v", category, body)
	}
}

func TestOnlyGenericCoreutilsToolIsExposed(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	if names := server.ToolNames(); len(names) != 1 || names[0] != "coreutils_run" {
		t.Fatalf("unexpected exposed tools: %v", names)
	}
	for _, name := range append(WriteCapableTools, "cat", "grep", "sh", "rm") {
		params, _ := json.Marshal(mcpproto.CallToolParams{Name: name, Arguments: json.RawMessage(`{}`)})
		body := map[string]any{}
		_ = json.Unmarshal([]byte(server.callTool(context.Background(), params).Text()), &body)
		expectError(t, body, mcpproto.ErrorUnknownTool)
	}
}

func TestCoreutilsRunValidatesAndExecutes(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	body := call(t, server, map[string]any{"command": "sort", "args": []string{"--reverse"}, "stdin": "pear\napple\norange\n"})
	if body["success"] != true || body["stdout"] != "pear\norange\napple\n" || body["command"] != "sort" {
		t.Fatalf("unexpected sort result: %v", body)
	}
	expectError(t, call(t, server, map[string]any{"command": "rm", "args": []string{"-rf", "/"}}), mcpproto.ErrorPermissionDenied)
	expectError(t, call(t, server, map[string]any{"command": "sort", "args": []string{"--output", "x"}}), mcpproto.ErrorInvalidArguments)
	expectError(t, call(t, server, map[string]any{"command": "head", "args": []string{"-n", "-1"}}), mcpproto.ErrorInvalidArguments)
}

func TestCoreutilsRunEnforcesInputSchema(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	expectError(t, call(t, server, map[string]any{}), mcpproto.ErrorInvalidArguments)
	expectError(t, call(t, server, map[string]any{"command": "sort", "shell": "rm -rf /"}), mcpproto.ErrorInvalidArguments)
	overLimit := strings.Repeat("a", 64<<10+1)
	expectError(t, call(t, server, map[string]any{"command": "sort", "stdin": overLimit}), mcpproto.ErrorInvalidArguments)
}

func TestServeHandlesLifecycleOverStdio(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n")
	output := &strings.Builder{}
	if err := server.Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(output.String()), "\n"); len(lines) != 2 {
		t.Fatalf("expected 2 responses, got %q", output.String())
	}
}

func TestNewRejectsMissingWorkspace(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "missing"), DefaultLimits(), nil); err == nil {
		t.Fatal("expected a missing workspace to be rejected")
	}
}
