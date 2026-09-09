package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

func newTestServer(t *testing.T, workspace string) *Server {
	t.Helper()
	server, err := New(workspace, DefaultLimits(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

func call(t *testing.T, server *Server, name, arguments string) map[string]any {
	t.Helper()
	params, _ := json.Marshal(mcpproto.CallToolParams{Name: name, Arguments: json.RawMessage(arguments)})
	result := server.callTool(context.Background(), params)
	body := map[string]any{}
	if err := json.Unmarshal([]byte(result.Text()), &body); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return body
}

func TestMCPToolsAreExposed(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	names := server.ToolNames()
	required := []string{"coreutils_run", "pwd", "read_file", "write_file", "find_paths", "search_file", "grep_text"}
	for _, name := range required {
		found := false
		for _, candidate := range names {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tool %q not exposed: %v", name, names)
		}
	}
	for _, name := range append(WriteCapableTools, "sh") {
		if body := call(t, server, name, `{}`); body["error"] != mcpproto.ErrorUnknownTool {
			t.Fatalf("%s: %v", name, body)
		}
	}
}

func TestPwdReturnsLogicalWorkspacePath(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	body := call(t, newTestServer(t, workspace), "pwd", `{}`)
	if body["success"] != true || body["output"] != "/project" {
		t.Fatalf("unexpected pwd result: %v", body)
	}
}

func TestCoreutilsRunExecutesAndRejectsUnsafeInput(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	body := call(t, server, "coreutils_run", `{"command":"sort","stdin":"b\na\n"}`)
	if body["success"] != true || body["stdout"] != "a\nb\n" {
		t.Fatalf("unexpected sort result: %v", body)
	}
	if body := call(t, server, "coreutils_run", `{"command":"rm"}`); body["error"] != mcpproto.ErrorPermissionDenied {
		t.Fatalf("unsafe command: %v", body)
	}
	if body := call(t, server, "coreutils_run", `{"command":"sort","shell":"rm -rf /"}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("invalid schema: %v", body)
	}
	if body := call(t, server, "coreutils_run", `{"command":"head","args":["-n","-1"]}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("invalid args: %v", body)
	}
}

func TestServeHandlesLifecycleOverStdio(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n")
	output := &strings.Builder{}
	if err := server.Serve(context.Background(), input, output); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(output.String()), "\n"); len(lines) != 2 {
		t.Fatalf("unexpected output: %q", output.String())
	}
}
