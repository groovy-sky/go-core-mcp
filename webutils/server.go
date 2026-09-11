package webutils

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"

	"github.com/groovy-sky/groovy-agent/internal/jsonschema"
	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

const (
	toolNameBrowseURL = "browse_url"
)

type Server struct {
	logger  *log.Logger
	limits  Limits
	browser Browser

	writeMutex sync.Mutex
}

func NewServer(limits Limits, browser Browser, logger *log.Logger) *Server {
	if browser == nil {
		browser = NewChromiumBrowser(limits)
	}
	return &Server{
		logger:  logger,
		limits:  limits,
		browser: browser,
	}
}

func (s *Server) ToolNames() []string {
	return []string{toolNameBrowseURL}
}

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
		result := mcpproto.InitializeResult{
			ProtocolVersion: mcpproto.Version,
			Capabilities:    mcpproto.ServerCapabilities{Tools: &mcpproto.ToolsCapability{}},
			ServerInfo:      mcpproto.Implementation{Name: "webutils-mcp", Version: "1.0.0"},
		}
		s.respondResult(output, request.ID, result)
	case "notifications/initialized", "notifications/cancelled":
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
	return mcpproto.ListToolsResult{
		Tools: []mcpproto.Tool{{
			Name:        toolNameBrowseURL,
			Description: "Browse one public HTTPS page with a fresh headless Chromium instance, optionally execute sequential CSS-selector actions after navigation, and return bounded extracted content, visible text, links, and optional screenshot.",
			InputSchema: mustJSON(inputSchema(s.limits)),
		}},
	}
}

func inputSchema(limits Limits) map[string]any {
	limits = normalizeLimits(limits)
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Public HTTPS URL to browse.",
				"maxLength":   maxURLLength,
			},
			"max_text_chars": map[string]any{
				"type":        "integer",
				"description": "Maximum content and visible_text characters in the result.",
				"minimum":     1,
				"maximum":     maxAllowedTextChars,
			},
			"capture_screenshot": map[string]any{
				"type":        "boolean",
				"description": "Capture a PNG screenshot and return it as a separate MCP image content block.",
				"default":     false,
			},
			"screenshot_mode": map[string]any{
				"type":        "string",
				"description": "Screenshot capture mode when capture_screenshot is true.",
				"enum":        []any{screenshotModeViewport, screenshotModeFullPage},
			},
			"actions": map[string]any{
				"type":        "array",
				"description": "Optional browser actions to execute sequentially after navigation and before content extraction or screenshot capture.",
				"maxItems":    limits.MaxActions,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type": map[string]any{
							"type":        "string",
							"description": "Browser action type.",
							"minLength":   1,
							"maxLength":   limits.MaxActionTypeChars,
						},
						"selector": map[string]any{
							"type":        "string",
							"description": "CSS selector for the target element.",
							"minLength":   1,
							"maxLength":   limits.MaxSelectorChars,
						},
						"value": map[string]any{
							"type":        "string",
							"description": "Action value for set_value and type actions.",
							"maxLength":   limits.MaxActionValueChars,
						},
					},
					"required":             []any{"type", "selector"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []any{"url"},
		"additionalProperties": false,
	}
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) mcpproto.CallToolResult {
	params := mcpproto.CallToolParams{}
	if err := json.Unmarshal(raw, &params); err != nil {
		return errorResult(mcpproto.ErrorInvalidArguments, "tool call parameters are not a JSON object")
	}
	if params.Name != toolNameBrowseURL {
		return errorResult(mcpproto.ErrorUnknownTool, "tool is not available")
	}
	arguments, err := jsonschema.ValidateRaw(inputSchema(s.limits), params.Arguments)
	if err != nil {
		return errorResult(mcpproto.ErrorInvalidArguments, err.Error())
	}
	request := BrowseRequest{
		URL:               arguments["url"].(string),
		MaxTextChars:      optionalInt(arguments, "max_text_chars"),
		CaptureScreenshot: optionalBool(arguments, "capture_screenshot"),
		ScreenshotMode:    optionalString(arguments, "screenshot_mode"),
		Actions:           optionalBrowserActions(arguments, "actions"),
	}
	result, err := s.browser.Browse(ctx, request)
	if err != nil {
		category, message := classifyError(err)
		return errorResult(category, message)
	}
	body := map[string]any{
		"success":           true,
		"final_url":         result.FinalURL,
		"title":             result.Title,
		"content":           result.Content,
		"content_format":    result.ContentFormat,
		"extraction_method": result.ExtractionMethod,
		"visible_text":      result.VisibleText,
		"links":             result.Links,
		"truncated":         result.Truncated,
	}
	content := []mcpproto.Content{{Type: "text", Text: encode(body)}}
	if len(result.ScreenshotPNG) > 0 {
		content = append(content, mcpproto.Content{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(result.ScreenshotPNG),
			MIMEType: "image/png",
		})
	}
	return mcpproto.CallToolResult{
		Content: content,
	}
}

func optionalInt(arguments map[string]any, key string) int {
	number, ok := jsonschema.Number(arguments[key])
	if !ok {
		return 0
	}
	return number
}

func optionalBool(arguments map[string]any, key string) bool {
	boolean, ok := arguments[key].(bool)
	return ok && boolean
}

func optionalString(arguments map[string]any, key string) string {
	value, ok := arguments[key].(string)
	if !ok {
		return ""
	}
	return value
}

func optionalBrowserActions(arguments map[string]any, key string) []BrowserAction {
	items, ok := arguments[key].([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	actions := make([]BrowserAction, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		actions = append(actions, BrowserAction{
			Type:     optionalString(object, "type"),
			Selector: optionalString(object, "selector"),
			Value:    optionalString(object, "value"),
		})
	}
	return actions
}

func classifyError(err error) (string, string) {
	if err == nil {
		return mcpproto.ErrorToolError, "tool execution failed"
	}
	switch {
	case errors.Is(err, errURLRequired), errors.Is(err, errURLTooLong), errors.Is(err, errURLMustBeHTTPS), errors.Is(err, errURLHostRequired), errors.Is(err, errURLUserinfoNotAllowed):
		return "invalid_url", err.Error()
	case errors.Is(err, errHostNotPublic), errors.Is(err, errHostNoPublicAddress):
		return "disallowed_destination", err.Error()
	case errors.Is(err, errScreenshotTooLarge):
		return mcpproto.ErrorResultTooLarge, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return mcpproto.ErrorTimeout, "browse operation exceeded its time budget"
	default:
		message := "browse operation failed"
		if strings.TrimSpace(err.Error()) != "" {
			message = err.Error()
		}
		return mcpproto.ErrorToolError, message
	}
}

func encode(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"success":false,"error":"tool_error","message":"result could not be encoded"}`
	}
	return string(encoded)
}

func errorResult(category, message string) mcpproto.CallToolResult {
	body := map[string]any{"success": false, "error": category, "message": message}
	return mcpproto.CallToolResult{
		Content: []mcpproto.Content{{Type: "text", Text: encode(body)}},
		IsError: true,
	}
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
