package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

func newTestHTTPServer(t *testing.T, opts HTTPOptions) (*Server, *httptest.Server) {
	t.Helper()
	server := newTestServer(t, t.TempDir())
	httpServer := httptest.NewServer(server.HTTPHandler(opts))
	t.Cleanup(httpServer.Close)
	return server, httpServer
}

func postJSON(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

func TestHTTPHandlesFullLifecycle(t *testing.T) {
	server, httpServer := newTestHTTPServer(t, HTTPOptions{})
	_ = server

	initResp := postJSON(t, httpServer.URL+DefaultHTTPPath, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: expected 200, got %d", initResp.StatusCode)
	}
	if ct := initResp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("initialize: expected application/json content type, got %q", ct)
	}
	message := mcpproto.Message{}
	if err := json.Unmarshal([]byte(readBody(t, initResp)), &message); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	result := mcpproto.InitializeResult{}
	if err := json.Unmarshal(message.Result, &result); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if result.Capabilities.Tools == nil {
		t.Fatal("server must advertise tool capability")
	}

	notifyResp := postJSON(t, httpServer.URL+DefaultHTTPPath, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if notifyResp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: expected 202, got %d", notifyResp.StatusCode)
	}
	if body := readBody(t, notifyResp); body != "" {
		t.Fatalf("notification: expected empty body, got %q", body)
	}

	listResp := postJSON(t, httpServer.URL+DefaultHTTPPath, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: expected 200, got %d", listResp.StatusCode)
	}
	listed := mcpproto.Message{}
	if err := json.Unmarshal([]byte(readBody(t, listResp)), &listed); err != nil {
		t.Fatalf("decode tools/list response: %v", err)
	}
	tools := mcpproto.ListToolsResult{}
	if err := json.Unmarshal(listed.Result, &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	if len(tools.Tools) != len(server.ToolNames()) {
		t.Fatalf("expected %d tools, got %d", len(server.ToolNames()), len(tools.Tools))
	}

	callResp := postJSON(t, httpServer.URL+DefaultHTTPPath, "", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"coreutils_run","arguments":{"command":"wc","stdin":"one two\n"}}}`)
	if callResp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call: expected 200, got %d", callResp.StatusCode)
	}
	called := mcpproto.Message{}
	if err := json.Unmarshal([]byte(readBody(t, callResp)), &called); err != nil {
		t.Fatalf("decode tools/call response: %v", err)
	}
	callResult := mcpproto.CallToolResult{}
	if err := json.Unmarshal(called.Result, &callResult); err != nil {
		t.Fatalf("decode call result: %v", err)
	}
	if callResult.IsError {
		t.Fatalf("coreutils call failed: %s", callResult.Text())
	}
	body := map[string]any{}
	if err := json.Unmarshal([]byte(callResult.Text()), &body); err != nil {
		t.Fatalf("decode coreutils payload: %v", err)
	}
	if body["success"] != true {
		t.Fatalf("expected coreutils success, got %v", body)
	}
}

func TestHTTPRejectsMalformedJSON(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{})
	resp := postJSON(t, httpServer.URL+DefaultHTTPPath, "", "not json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected JSON-RPC parse error wrapped in HTTP 200, got %d", resp.StatusCode)
	}
	message := mcpproto.Message{}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &message); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if message.Error == nil || message.Error.Code != mcpproto.CodeParseError {
		t.Fatalf("expected parse error, got %+v", message.Error)
	}
}

func TestHTTPRejectsUnknownMethod(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{})
	resp := postJSON(t, httpServer.URL+DefaultHTTPPath, "", `{"jsonrpc":"2.0","id":9,"method":"nope"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	message := mcpproto.Message{}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &message); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if message.Error == nil || message.Error.Code != mcpproto.CodeMethodNotFound {
		t.Fatalf("expected method not found, got %+v", message.Error)
	}
}

func TestHTTPRejectsBatchedRequests(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{})
	resp := postJSON(t, httpServer.URL+DefaultHTTPPath, "", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for batched request, got %d", resp.StatusCode)
	}
}

func TestHTTPRejectsWrongContentType(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{})
	req, err := http.NewRequest(http.MethodPost, httpServer.URL+DefaultHTTPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPRejectsMissingContentType(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{})
	req, err := http.NewRequest(http.MethodPost, httpServer.URL+DefaultHTTPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Deliberately do not set Content-Type: the transport must not guess
	// that an untyped body is JSON-RPC.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for missing Content-Type, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPRejectsGETAndDELETE(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{})
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, err := http.NewRequest(method, httpServer.URL+DefaultHTTPPath, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s: expected 405, got %d", method, resp.StatusCode)
		}
	}
}

func TestHTTPBearerTokenIsEnforced(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{BearerToken: "secret-token"})

	unauthorized := postJSON(t, httpServer.URL+DefaultHTTPPath, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", unauthorized.StatusCode)
	}
	unauthorized.Body.Close()

	wrong := postJSON(t, httpServer.URL+DefaultHTTPPath, "wrong-token", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", wrong.StatusCode)
	}
	wrong.Body.Close()

	authorized := postJSON(t, httpServer.URL+DefaultHTTPPath, "secret-token", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if authorized.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with correct token, got %d", authorized.StatusCode)
	}
	authorized.Body.Close()
}

func TestHTTPBearerSchemeIsCaseInsensitive(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{BearerToken: "secret-token"})

	req, err := http.NewRequest(http.MethodPost, httpServer.URL+DefaultHTTPPath,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with lowercase scheme, got %d", resp.StatusCode)
	}
}

func TestHTTPToolCallRejectsForbiddenCommand(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{})
	resp := postJSON(t, httpServer.URL+DefaultHTTPPath, "",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"coreutils_run","arguments":{"command":"cat","stdin":"ignored"}}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	message := mcpproto.Message{}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &message); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	callResult := mcpproto.CallToolResult{}
	if err := json.Unmarshal(message.Result, &callResult); err != nil {
		t.Fatalf("decode call result: %v", err)
	}
	if !callResult.IsError {
		t.Fatalf("expected an error result, got %s", callResult.Text())
	}
	body := map[string]any{}
	if err := json.Unmarshal([]byte(callResult.Text()), &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if body["error"] != mcpproto.ErrorPermissionDenied {
		t.Fatalf("expected permission_denied, got %v", body["error"])
	}
}

func TestHTTPRejectsOversizedBody(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{})
	oversized := strings.Repeat("a", maxHTTPRequestBytes+1)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cat","arguments":{"path":"` + oversized + `"}}}`
	resp := postJSON(t, httpServer.URL+DefaultHTTPPath, "", body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPUsesCustomPath(t *testing.T) {
	_, httpServer := newTestHTTPServer(t, HTTPOptions{Path: "/custom/mcp"})
	resp := postJSON(t, httpServer.URL+"/custom/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on custom path, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPHandlerNormalizesPathWithoutLeadingSlash(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	httpServer := httptest.NewServer(server.HTTPHandler(HTTPOptions{Path: "custom/mcp"}))
	t.Cleanup(httpServer.Close)

	resp := postJSON(t, httpServer.URL+"/custom/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on normalized path, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
