package webutils

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

type fakeBrowser struct {
	result BrowseResult
	err    error
	last   BrowseRequest
}

func (f *fakeBrowser) Browse(_ context.Context, req BrowseRequest) (BrowseResult, error) {
	f.last = req
	return f.result, f.err
}

func TestToolSchemaAndListWiring(t *testing.T) {
	server := NewServer(DefaultLimits(), &fakeBrowser{}, log.New(io.Discard, "", 0))
	list := server.listTools()
	if len(list.Tools) != 1 || list.Tools[0].Name != toolNameBrowseURL {
		t.Fatalf("unexpected tools list: %+v", list.Tools)
	}
	schema := map[string]any{}
	if err := json.Unmarshal(list.Tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("expected closed schema, got %+v", schema)
	}
}

func TestCallToolSuccess(t *testing.T) {
	browser := &fakeBrowser{result: BrowseResult{
		FinalURL:    "https://example.com",
		Title:       "Example",
		VisibleText: "hello",
		Links:       []Link{{Text: "about", URL: "https://example.com/about"}},
		Truncated:   true,
	}}
	server := NewServer(DefaultLimits(), browser, log.New(io.Discard, "", 0))
	raw := json.RawMessage(`{"name":"browse_url","arguments":{"url":"https://example.com","max_text_chars":123}}`)
	result := server.callTool(context.Background(), raw)
	if result.IsError {
		t.Fatalf("callTool returned error result: %+v", result)
	}
	body := map[string]any{}
	if err := json.Unmarshal([]byte(result.Text()), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["success"] != true || body["final_url"] != "https://example.com" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if browser.last.MaxTextChars != 123 {
		t.Fatalf("expected max_text_chars to be forwarded, got %d", browser.last.MaxTextChars)
	}
}

func TestCallToolRejectsUnknownArguments(t *testing.T) {
	server := NewServer(DefaultLimits(), &fakeBrowser{}, log.New(io.Discard, "", 0))
	raw := json.RawMessage(`{"name":"browse_url","arguments":{"url":"https://example.com","capture_screenshot":true}}`)
	result := server.callTool(context.Background(), raw)
	if !result.IsError {
		t.Fatal("expected schema validation error")
	}
	body := map[string]any{}
	if err := json.Unmarshal([]byte(result.Text()), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected invalid_arguments, got %+v", body)
	}
}

func TestClassifyError(t *testing.T) {
	code, _ := classifyError(errURLMustBeHTTPS)
	if code != "invalid_url" {
		t.Fatalf("expected invalid_url, got %q", code)
	}
	code, _ = classifyError(errHostNotPublic)
	if code != "disallowed_destination" {
		t.Fatalf("expected disallowed_destination, got %q", code)
	}
	code, _ = classifyError(context.DeadlineExceeded)
	if code != mcpproto.ErrorTimeout {
		t.Fatalf("expected timeout, got %q", code)
	}
	code, _ = classifyError(errors.New("boom"))
	if code != mcpproto.ErrorToolError {
		t.Fatalf("expected tool_error, got %q", code)
	}
}

func TestClampString(t *testing.T) {
	clamped, truncated := clampString("abcdef", 4)
	if clamped != "abcd" || !truncated {
		t.Fatalf("unexpected clamp result: %q %t", clamped, truncated)
	}
}
