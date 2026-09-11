package webutils

import (
	"context"
	"encoding/base64"
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
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected schema properties map, got %+v", schema["properties"])
	}
	if _, ok := properties["capture_screenshot"]; !ok {
		t.Fatalf("expected capture_screenshot property, got %+v", properties)
	}
	mode, ok := properties["screenshot_mode"].(map[string]any)
	if !ok {
		t.Fatalf("expected screenshot_mode property, got %+v", properties["screenshot_mode"])
	}
	if _, ok := mode["enum"]; !ok {
		t.Fatalf("expected screenshot_mode enum, got %+v", mode)
	}
}

func TestCallToolSuccess(t *testing.T) {
	browser := &fakeBrowser{result: BrowseResult{
		FinalURL:         "https://example.com",
		Title:            "Example",
		Content:          "# Example",
		ContentFormat:    contentFormatMarkdown,
		ExtractionMethod: extractionReadability,
		VisibleText:      "hello",
		Links:            []Link{{Text: "about", URL: "https://example.com/about"}},
		Truncated:        true,
		ScreenshotPNG:    []byte("png-bytes"),
	}}
	server := NewServer(DefaultLimits(), browser, log.New(io.Discard, "", 0))
	raw := json.RawMessage(`{"name":"browse_url","arguments":{"url":"https://example.com","max_text_chars":123,"capture_screenshot":true,"screenshot_mode":"full_page"}}`)
	result := server.callTool(context.Background(), raw)
	if result.IsError {
		t.Fatalf("callTool returned error result: %+v", result)
	}
	if len(result.Content) != 2 {
		t.Fatalf("expected text and image content blocks, got %+v", result.Content)
	}
	if result.Content[1].Type != "image" || result.Content[1].MIMEType != "image/png" {
		t.Fatalf("expected PNG image content block, got %+v", result.Content[1])
	}
	if want := base64.StdEncoding.EncodeToString([]byte("png-bytes")); result.Content[1].Data != want {
		t.Fatalf("expected encoded image data %q, got %q", want, result.Content[1].Data)
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
	if !browser.last.CaptureScreenshot {
		t.Fatal("expected capture_screenshot to be forwarded")
	}
	if browser.last.ScreenshotMode != screenshotModeFullPage {
		t.Fatalf("expected screenshot_mode to be forwarded, got %q", browser.last.ScreenshotMode)
	}
}

func TestCallToolRejectsUnknownArguments(t *testing.T) {
	server := NewServer(DefaultLimits(), &fakeBrowser{}, log.New(io.Discard, "", 0))
	raw := json.RawMessage(`{"name":"browse_url","arguments":{"url":"https://example.com","unexpected":true}}`)
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
	code, _ = classifyError(errScreenshotTooLarge)
	if code != mcpproto.ErrorResultTooLarge {
		t.Fatalf("expected result_too_large, got %q", code)
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
