package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
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

func TestOnlySafeMCPToolsAreExposed(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	if names := server.ToolNames(); len(names) != 18 {
		t.Fatalf("unexpected tools: %v", names)
	}
	for _, name := range []string{
		"cat", "coreutils_run", "cp", "find", "grep", "grep_file", "grep_text",
		"head", "ls", "mkdir", "mv", "pwd", "read_file", "rm", "rmdir",
		"tail", "touch", "write_file",
	} {
		if !sort.StringsAreSorted(server.ToolNames()) {
			t.Fatalf("tool names must be sorted: %v", server.ToolNames())
		}
		found := false
		for _, candidate := range server.ToolNames() {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected tool %q in %v", name, server.ToolNames())
		}
	}
	for _, name := range append(WriteCapableTools, "sh", "unlink", "tee") {
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

func TestReadAndGrepRejectBinaryFiles(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "data.bin"), []byte{0xff, 0x00, 0x01}, 0o600); err != nil {
		t.Fatalf("write binary file: %v", err)
	}
	server := newTestServer(t, workspace)

	for _, name := range []string{"cat", "read_file", "grep", "grep_file"} {
		body := call(t, server, name, `{"path":"data.bin","pattern":"x"}`)
		if name == "cat" || name == "read_file" {
			body = call(t, server, name, `{"path":"data.bin"}`)
		}
		if body["error"] != mcpproto.ErrorInvalidArguments {
			t.Fatalf("%s: expected invalid_arguments, got %v", name, body)
		}
		if !strings.Contains(body["message"].(string), "valid UTF-8 text") {
			t.Fatalf("%s: unexpected message %v", name, body["message"])
		}
	}
}

func TestWriteFileCreatesAndOverwritesFiles(t *testing.T) {
	workspace := t.TempDir()
	server := newTestServer(t, workspace)

	created := call(t, server, "write_file", `{"path":"notes/todo.txt","content":"hello"}`)
	if created["success"] != false || created["error"] != mcpproto.ErrorToolError {
		t.Fatalf("expected missing parent failure, got %v", created)
	}

	if err := os.Mkdir(filepath.Join(workspace, "notes"), 0o755); err != nil {
		t.Fatalf("mkdir notes: %v", err)
	}
	body := call(t, server, "write_file", `{"path":"notes/todo.txt","content":"hello"}`)
	if body["success"] != true || body["output"] != "notes/todo.txt" {
		t.Fatalf("unexpected create result: %v", body)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "notes", "todo.txt"))
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(content) != "hello" {
		t.Fatalf("unexpected written content %q", content)
	}

	conflict := call(t, server, "write_file", `{"path":"notes/todo.txt","content":"updated"}`)
	if conflict["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected overwrite guard, got %v", conflict)
	}

	replaced := call(t, server, "write_file", `{"path":"notes/todo.txt","content":"updated","overwrite":true}`)
	if replaced["success"] != true {
		t.Fatalf("unexpected overwrite result: %v", replaced)
	}
	content, err = os.ReadFile(filepath.Join(workspace, "notes", "todo.txt"))
	if err != nil {
		t.Fatalf("read overwritten file: %v", err)
	}
	if string(content) != "updated" {
		t.Fatalf("unexpected overwritten content %q", content)
	}
}

func TestFindSearchesFilesAndDirectories(t *testing.T) {
	workspace := t.TempDir()
	for _, path := range []string{
		"docs",
		"docs/api",
		"src",
	} {
		if err := os.Mkdir(filepath.Join(workspace, path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	for path, content := range map[string]string{
		"docs/guide.md":         "guide",
		"docs/api/openapi.yaml": "spec",
		"src/main.go":           "package main\n",
	} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	server := newTestServer(t, workspace)

	body := call(t, server, "find", `{"path":"docs","pattern":"api","fixed":true}`)
	if body["success"] != true || body["output"] != "docs/api/\ndocs/api/openapi.yaml\n" {
		t.Fatalf("unexpected find result: %v", body)
	}

	onlyDirs := call(t, server, "find", `{"pattern":"docs","fixed":true,"files":false,"directories":true}`)
	if onlyDirs["success"] != true || onlyDirs["output"] != "docs/\ndocs/api/\n" {
		t.Fatalf("unexpected directory search: %v", onlyDirs)
	}

	invalid := call(t, server, "find", `{"pattern":"docs","files":false,"directories":false}`)
	if invalid["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected invalid find flags, got %v", invalid)
	}
}

func TestGrepFileAndTextReturnLineNumbers(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("alpha\nbeta\nalpha two\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	server := newTestServer(t, workspace)

	fileBody := call(t, server, "grep_file", `{"path":"README.md","pattern":"alpha","fixed":true}`)
	if fileBody["success"] != true || fileBody["output"] != "1:alpha\n3:alpha two\n" {
		t.Fatalf("unexpected grep_file result: %v", fileBody)
	}

	textBody := call(t, server, "grep_text", `{"text":"zero\none\nONE\n","pattern":"one","fixed":true,"ignore_case":true}`)
	if textBody["success"] != true || textBody["output"] != "2:one\n3:ONE\n" {
		t.Fatalf("unexpected grep_text result: %v", textBody)
	}
}

func TestReadFileAliasReturnsContent(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	server := newTestServer(t, workspace)

	body := call(t, server, "read_file", `{"path":"README.md"}`)
	if body["success"] != true || body["output"] != "hello\nworld\n" {
		t.Fatalf("unexpected read_file result: %v", body)
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
