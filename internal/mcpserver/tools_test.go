package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

func TestWriteFileRequiresExplicitOverwriteOrAppend(t *testing.T) {
	workspace := t.TempDir()
	server := newTestServer(t, workspace)

	body := call(t, server, "write_file", `{"path":"notes.txt","content":"one\n"}`)
	if body["success"] != true {
		t.Fatalf("write_file create failed: %v", body)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "notes.txt"))
	if err != nil {
		t.Fatalf("read notes.txt: %v", err)
	}
	if string(content) != "one\n" {
		t.Fatalf("unexpected create content: %q", string(content))
	}

	if body := call(t, server, "write_file", `{"path":"notes.txt","content":"two\n"}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected overwrite rejection, got %v", body)
	}

	body = call(t, server, "write_file", `{"path":"notes.txt","content":"two\n","overwrite":true}`)
	if body["success"] != true {
		t.Fatalf("overwrite failed: %v", body)
	}
	content, err = os.ReadFile(filepath.Join(workspace, "notes.txt"))
	if err != nil {
		t.Fatalf("read overwritten notes.txt: %v", err)
	}
	if string(content) != "two\n" {
		t.Fatalf("unexpected overwrite content: %q", string(content))
	}

	body = call(t, server, "write_file", `{"path":"notes.txt","content":"three\n","append":true}`)
	if body["success"] != true {
		t.Fatalf("append failed: %v", body)
	}
	content, err = os.ReadFile(filepath.Join(workspace, "notes.txt"))
	if err != nil {
		t.Fatalf("read appended notes.txt: %v", err)
	}
	if string(content) != "two\nthree\n" {
		t.Fatalf("unexpected append content: %q", string(content))
	}

	if body := call(t, server, "write_file", `{"path":"notes.txt","content":"x","append":true,"overwrite":true}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected append+overwrite rejection, got %v", body)
	}
}

func TestWriteFileAppliesBoundsAndPathConfinement(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	if body := call(t, server, "write_file", `{"path":"../outside.txt","content":"x"}`); body["error"] != mcpproto.ErrorWorkspaceViolation {
		t.Fatalf("expected workspace violation, got %v", body)
	}
	oversized := strings.Repeat("a", DefaultLimits().MaxFileWriteBytes+1)
	body := call(t, server, "write_file", `{"path":"big.txt","content":"`+oversized+`"}`)
	if body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected oversized content rejection, got %v", body)
	}
}

func TestFindSearchesRecursivelyWithTypesAndLimits(t *testing.T) {
	workspace := t.TempDir()
	mustMkdir(t, filepath.Join(workspace, "a", "b"))
	mustWrite(t, filepath.Join(workspace, "a", "alpha.txt"), "alpha")
	mustWrite(t, filepath.Join(workspace, "a", "beta.md"), "beta")
	mustWrite(t, filepath.Join(workspace, "a", "b", "bravo.md"), "bravo")

	server := newTestServer(t, workspace)
	body := call(t, server, "find", `{"path":"a","name":"b","max_results":10}`)
	if body["success"] != true {
		t.Fatalf("find failed: %v", body)
	}
	output, _ := body["output"].(string)
	if !strings.Contains(output, "a/b/") {
		t.Fatalf("expected directory marker in output, got %q", output)
	}
	if !strings.Contains(output, "a/beta.md") {
		t.Fatalf("expected matching file in output, got %q", output)
	}

	body = call(t, server, "find", `{"path":"a","name":"*.md","match_mode":"glob","max_results":1}`)
	if body["success"] != true {
		t.Fatalf("glob find failed: %v", body)
	}
	if body["truncated"] != true {
		t.Fatalf("expected truncated=true for max_results cap, got %v", body)
	}
	output, _ = body["output"].(string)
	if output == "" {
		t.Fatal("expected at least one glob match")
	}

	if body := call(t, server, "find", `{"path":"../","name":"x"}`); body["error"] != mcpproto.ErrorWorkspaceViolation {
		t.Fatalf("expected workspace violation for find path, got %v", body)
	}
}

func TestGrepSupportsPathOrTextInputs(t *testing.T) {
	workspace := t.TempDir()
	mustWrite(t, filepath.Join(workspace, "README.md"), "TODO one\nskip\nTODO two\n")
	server := newTestServer(t, workspace)

	body := call(t, server, "grep", `{"path":"README.md","pattern":"TODO"}`)
	if body["success"] != true {
		t.Fatalf("grep path failed: %v", body)
	}
	output, _ := body["output"].(string)
	if !strings.Contains(output, "1:TODO one") || !strings.Contains(output, "3:TODO two") {
		t.Fatalf("unexpected path grep output: %q", output)
	}

	body = call(t, server, "grep", `{"text":"TODO one\nskip\nTODO two\n","pattern":"TODO","max_matches":1}`)
	if body["success"] != true {
		t.Fatalf("grep text failed: %v", body)
	}
	if body["truncated"] != true {
		t.Fatalf("expected text grep truncation at max_matches, got %v", body)
	}
	output, _ = body["output"].(string)
	if output != "1:TODO one\n" {
		t.Fatalf("unexpected text grep output: %q", output)
	}

	if body := call(t, server, "grep", `{"path":"README.md","text":"TODO","pattern":"TODO"}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected path+text rejection, got %v", body)
	}
	if body := call(t, server, "grep", `{"pattern":"TODO"}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected missing input rejection, got %v", body)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
