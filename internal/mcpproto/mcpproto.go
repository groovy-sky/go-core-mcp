// Package mcpproto contains the JSON-RPC 2.0 and Model Context Protocol
// message types exchanged over the stdio transport.
package mcpproto

import "encoding/json"

// Version is the MCP protocol revision implemented by this repository.
const Version = "2024-11-05"

// Standard JSON-RPC error codes plus the MCP specific ones used here.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Message is a JSON-RPC request, notification, or response.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Implementation identifies a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities is sent by the client during initialization.
type ClientCapabilities struct{}

// ToolsCapability advertises tool support.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// ServerCapabilities is returned by the server during initialization.
type ServerCapabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

// InitializeParams are the parameters of the initialize request.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the response to the initialize request.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
}

// Tool is an MCP tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListToolsParams supports cursor based pagination.
type ListToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListToolsResult is a page of tool definitions.
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallToolParams asks the server to run a tool.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Content is a single content block of a tool result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

// CallToolResult is the outcome of a tool invocation.
type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Text returns the concatenated text content blocks.
func (r CallToolResult) Text() string {
	if len(r.Content) == 1 && r.Content[0].Type == "text" {
		return r.Content[0].Text
	}
	combined := ""
	for _, block := range r.Content {
		if block.Type != "text" {
			continue
		}
		combined += block.Text
	}
	return combined
}

// Tool error categories reported to the model.
const (
	ErrorUnknownTool        = "unknown_tool"
	ErrorInvalidArguments   = "invalid_arguments"
	ErrorWorkspaceViolation = "workspace_violation"
	ErrorPermissionDenied   = "permission_denied"
	ErrorTimeout            = "timeout"
	ErrorResultTooLarge     = "result_too_large"
	ErrorToolError          = "tool_error"
)
