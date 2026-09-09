package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func TestWriteFileCreateAndOverwriteBehavior(t *testing.T) {
	workspace := t.TempDir()
	server := newTestServer(t, workspace)

	body := call(t, server, "write_file", `{"path":"notes.txt","content":"hello"}`)
	if body["success"] != true {
		t.Fatalf("write create failed: %v", body)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "notes.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("unexpected created file content %q err=%v", string(data), err)
	}

	if body := call(t, server, "write_file", `{"path":"notes.txt","content":"again"}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected overwrite guard, got %v", body)
	}

	body = call(t, server, "write_file", `{"path":"notes.txt","content":"updated","overwrite":true}`)
	if body["success"] != true {
		t.Fatalf("write overwrite failed: %v", body)
	}
	data, err = os.ReadFile(filepath.Join(workspace, "notes.txt"))
	if err != nil || string(data) != "updated" {
		t.Fatalf("unexpected overwritten file content %q err=%v", string(data), err)
	}
}

func TestFindPathsSearchesAndBoundsResults(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "src", "a.txt"), "a")
	mustWriteFile(t, filepath.Join(workspace, "src", "b.txt"), "b")
	if err := os.MkdirAll(filepath.Join(workspace, "src", "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, workspace)

	body := call(t, server, "find_paths", `{"root":"src","pattern":"\\.txt$","max_results":1}`)
	if body["success"] != true || body["truncated"] != true {
		t.Fatalf("expected bounded results, got %v", body)
	}
	if body := call(t, server, "find_paths", `{"root":"src","pattern":"subdir","fixed":true,"include_files":false,"include_directories":true}`); !strings.Contains(body["output"].(string), "src/subdir/") {
		t.Fatalf("expected directory match, got %v", body)
	}
	if body := call(t, server, "find_paths", `{"root":"src","pattern":"x","include_files":false,"include_directories":false}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected include guard, got %v", body)
	}
}

func TestSearchFileHandlesContextAndBoundaries(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "sample.txt"), "zero\nTODO one\nmiddle\nTODO two\n")
	mustWriteFile(t, filepath.Join(workspace, "binary.bin"), string([]byte{0xff, 0x00, 0x01}))
	mustWriteFile(t, filepath.Join(workspace, "large.txt"), strings.Repeat("a", DefaultLimits().MaxFileReadBytes+1))
	server := newTestServer(t, workspace)

	body := call(t, server, "search_file", `{"path":"sample.txt","pattern":"TODO","fixed":true,"context_lines":1}`)
	if body["success"] != true {
		t.Fatalf("search_file failed: %v", body)
	}
	output := body["output"].(string)
	if !strings.Contains(output, "2:TODO one") || !strings.Contains(output, "1-:zero") || !strings.Contains(output, "3-:middle") {
		t.Fatalf("missing context in output %q", output)
	}

	if body := call(t, server, "search_file", `{"path":"missing.txt","pattern":"x"}`); body["error"] != mcpproto.ErrorToolError {
		t.Fatalf("expected missing-file error, got %v", body)
	}
	if body := call(t, server, "search_file", `{"path":"binary.bin","pattern":"x"}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected binary-file guard, got %v", body)
	}
	if body := call(t, server, "search_file", `{"path":"large.txt","pattern":"a"}`); body["error"] != mcpproto.ErrorResultTooLarge {
		t.Fatalf("expected oversized-file guard, got %v", body)
	}
}

func TestGrepTextAndReadFile(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "readme.txt"), "alpha\nbeta\nALPHA\n")
	mustWriteFile(t, filepath.Join(workspace, "blob.bin"), string([]byte{0xff, 0x00}))
	server := newTestServer(t, workspace)

	body := call(t, server, "grep_text", `{"text":"alpha\nbeta\nALPHA\n","pattern":"alpha","ignore_case":true,"fixed":true,"max_matches":1}`)
	if body["success"] != true || body["truncated"] != true || !strings.Contains(body["output"].(string), "1:alpha") {
		t.Fatalf("grep_text failed: %v", body)
	}

	body = call(t, server, "read_file", `{"path":"readme.txt"}`)
	if body["success"] != true || body["output"] != "alpha\nbeta\nALPHA\n" {
		t.Fatalf("read_file failed: %v", body)
	}
	if body := call(t, server, "read_file", `{"path":"blob.bin"}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected binary-file guard, got %v", body)
	}
	if body := call(t, server, "read_file", `{"path":"readme.txt","max_bytes":5}`); body["error"] != mcpproto.ErrorResultTooLarge {
		t.Fatalf("expected max-bytes guard, got %v", body)
	}
}
