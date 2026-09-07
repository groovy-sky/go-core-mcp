package agent

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/llm"
	"github.com/groovy-sky/groovy-agent/internal/mcpclient"
	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
	"github.com/groovy-sky/groovy-agent/internal/mcpserver"
)

type fakeModel struct {
	replies  []llm.Message
	requests [][]llm.Message
	tools    [][]llm.Tool
}

func (f *fakeModel) Complete(_ context.Context, messages []llm.Message, tools []llm.Tool) (llm.Message, error) {
	f.requests = append(f.requests, append([]llm.Message{}, messages...))
	f.tools = append(f.tools, tools)
	if len(f.replies) == 0 {
		return llm.Message{Role: "assistant", Content: "done"}, nil
	}
	reply := f.replies[0]
	f.replies = f.replies[1:]
	return reply, nil
}

// newSession wires a session to a real in-process coreutils MCP server.
func newSession(t *testing.T, workspace, prompt string, model modelClient) (*Session, *strings.Builder) {
	t.Helper()
	server, err := mcpserver.New(workspace, mcpserver.DefaultLimits(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("mcpserver.New failed: %v", err)
	}
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	go func() {
		_ = server.Serve(context.Background(), serverReader, serverWriter)
		_ = serverWriter.Close()
	}()

	client := mcpclient.New(clientReader, clientWriter, nil)
	t.Cleanup(client.Close)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	output := &strings.Builder{}
	session := &Session{
		config:     Config{Workspace: workspace, Prompt: prompt},
		model:      model,
		mcp:        client,
		discovered: FilterDiscovered(tools, nil),
		logger:     log.New(io.Discard, "", 0),
		out:        output,
	}
	return session, output
}

func toolCall(id, name, arguments string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: arguments}}
}

func TestSelectProfileIsDeterministic(t *testing.T) {
	cases := map[string]string{
		"Use the date tool and report the exact current time.":     "date",
		"Find occurrences of \"TODO\" in README.md.":               "file_search",
		"Show the current workspace path and identify its README.": "file_inspection",
		"Read the beginning of README.md and summarize it.":        "file_inspection",
		"Count the unique sorted lines in the supplied text.":      "text_processing",
		"Give me the basename of docs/guide.md":                    "path_processing",
		"Explain quantum entanglement":                             "fallback",
	}
	for prompt, expected := range cases {
		profile := SelectProfile(prompt)
		if profile.Name != expected {
			t.Errorf("prompt %q selected %q, expected %q", prompt, profile.Name, expected)
		}
		if len(profile.Tools) > MaxExposedTools {
			t.Errorf("profile %q exposes %d tools", profile.Name, len(profile.Tools))
		}
	}
	// Selection must be stable across repeated calls.
	first := SelectProfile("Read README.md")
	second := SelectProfile("Read README.md")
	if first.Name != second.Name {
		t.Fatal("profile selection is not deterministic")
	}
}

func TestFilterDiscoveredDeniesUnexpectedTools(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	tools := []mcpproto.Tool{
		{Name: "coreutils_run", InputSchema: schema},
		{Name: "unlink", InputSchema: schema},
		{Name: "exec_command", InputSchema: schema},
	}
	kept := FilterDiscovered(tools, nil)
	if _, ok := kept["coreutils_run"]; !ok {
		t.Fatal("allowed tool was dropped")
	}
	if _, ok := kept["unlink"]; ok {
		t.Fatal("write-capable tool must be denied")
	}
	if _, ok := kept["exec_command"]; ok {
		t.Fatal("unexpected tool must be denied")
	}
}

func TestLoopExecutesToolCallAndPrintsFinalAnswer(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	model := &fakeModel{replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("call-1", "coreutils_run", `{"command":"head","args":["-n","1"],"stdin":"hello\nworld\n"}`)}},
		{Role: "assistant", Content: "The file starts with hello."},
	}}
	session, output := newSession(t, workspace, "Read the beginning of README.md and summarize it.", model)

	if err := session.Loop(context.Background()); err != nil {
		t.Fatalf("Loop failed: %v", err)
	}
	if strings.TrimSpace(output.String()) != "The file starts with hello." {
		t.Fatalf("unexpected final answer %q", output.String())
	}
	if len(model.requests) != 2 {
		t.Fatalf("expected 2 model rounds, got %d", len(model.requests))
	}
	second := model.requests[1]
	assistant := second[len(second)-2]
	toolResult := second[len(second)-1]
	if len(assistant.ToolCalls) == 0 {
		t.Fatal("the assistant tool-call message must precede the tool result")
	}
	if toolResult.Role != "tool" || toolResult.ToolCallID != "call-1" {
		t.Fatalf("unexpected tool message %+v", toolResult)
	}
	if !strings.Contains(toolResult.Content, `"success":true`) || !strings.Contains(toolResult.Content, "hello") {
		t.Fatalf("unexpected tool result %q", toolResult.Content)
	}
	for _, tool := range model.tools[0] {
		if tool.Type != "function" || tool.Function.Parameters == nil {
			t.Fatalf("tool %q was not adapted to the function envelope", tool.Function.Name)
		}
	}
}

func TestLoopStopsAtRoundLimit(t *testing.T) {
	workspace := t.TempDir()
	call := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("call-1", "coreutils_run", `{"command":"wc","stdin":""}`)}}
	model := &fakeModel{replies: []llm.Message{call, call, call}}
	session, output := newSession(t, workspace, "Show the current workspace path.", model)

	if err := session.Loop(context.Background()); err != nil {
		t.Fatalf("Loop failed: %v", err)
	}
	if strings.TrimSpace(output.String()) != RoundLimitMessage {
		t.Fatalf("unexpected output %q", output.String())
	}
	if len(model.requests) != MaxModelRounds {
		t.Fatalf("expected %d rounds, got %d", MaxModelRounds, len(model.requests))
	}
}

func TestLoopRejectsInvalidToolCalls(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	cases := []struct {
		name     string
		call     llm.ToolCall
		category string
	}{
		{"unknown tool", toolCall("c1", "unlink", `{"path":"README.md"}`), mcpproto.ErrorUnknownTool},
		{"non-exposed tool", toolCall("c1", "base64", `{"text":"hi"}`), mcpproto.ErrorUnknownTool},
		{"non-object arguments", toolCall("c1", "coreutils_run", `"text"`), mcpproto.ErrorInvalidArguments},
		{"schema violation", toolCall("c1", "coreutils_run", `{"command":"sort","danger":true}`), mcpproto.ErrorInvalidArguments},
		{"raised limit", toolCall("c1", "coreutils_run", `{"command":"head","args":["-n","100000"]}`), mcpproto.ErrorInvalidArguments},
		{"forbidden command", toolCall("c1", "coreutils_run", `{"command":"cat"}`), mcpproto.ErrorPermissionDenied},
		{"unsupported type", llm.ToolCall{ID: "c1", Type: "code", Function: llm.FunctionCall{Name: "coreutils_run"}}, mcpproto.ErrorInvalidArguments},
		{"missing id", llm.ToolCall{Type: "function", Function: llm.FunctionCall{Name: "coreutils_run", Arguments: `{"command":"sort"}`}}, mcpproto.ErrorInvalidArguments},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			model := &fakeModel{replies: []llm.Message{
				{Role: "assistant", ToolCalls: []llm.ToolCall{testCase.call}},
				{Role: "assistant", Content: "sorry"},
			}}
			session, _ := newSession(t, workspace, "Read README.md", model)
			if err := session.Loop(context.Background()); err != nil {
				t.Fatalf("Loop failed: %v", err)
			}
			second := model.requests[1]
			result := second[len(second)-1]
			if !strings.Contains(result.Content, testCase.category) {
				t.Fatalf("expected %q, got %q", testCase.category, result.Content)
			}
		})
	}
}

func TestToolCallBudgetIsEnforced(t *testing.T) {
	workspace := t.TempDir()
	session, _ := newSession(t, workspace, "Show the current workspace path.", &fakeModel{})
	exposed, _ := session.exposeProfile(SelectProfile(session.config.Prompt))

	for index := 0; index < MaxTotalToolCalls+1; index++ {
		message, err := session.runToolCall(context.Background(), exposed, toolCall("c", "coreutils_run", `{"command":"wc","stdin":""}`), 1, index)
		if err != nil {
			t.Fatalf("runToolCall failed: %v", err)
		}
		if index < MaxTotalToolCalls && !strings.Contains(message.Content, `"success":true`) {
			t.Fatalf("call %d should have succeeded: %s", index, message.Content)
		}
		if index == MaxTotalToolCalls && !strings.Contains(message.Content, mcpproto.ErrorToolError) {
			t.Fatalf("call %d should have exhausted the budget: %s", index, message.Content)
		}
	}
}

func TestDeleteRequestHasNoWriteTool(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "README.md")
	if err := os.WriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	model := &fakeModel{replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("c1", "unlink", `{"path":"README.md"}`)}},
		{Role: "assistant", Content: "Deleting files is not available."},
	}}
	session, output := newSession(t, workspace, "Delete README.md.", model)
	if err := session.Loop(context.Background()); err != nil {
		t.Fatalf("Loop failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file must not be mutated: %v", err)
	}
	if !strings.Contains(output.String(), "not available") {
		t.Fatalf("unexpected answer %q", output.String())
	}
}

func TestAdaptToolPreservesSchema(t *testing.T) {
	tool := mcpproto.Tool{
		Name:        "head",
		Description: "Read the first lines of a workspace file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"lines":{"type":"integer","minimum":1,"maximum":200}},"required":["path"],"additionalProperties":false}`),
	}
	adapted, err := AdaptTool(tool)
	if err != nil {
		t.Fatalf("AdaptTool failed: %v", err)
	}
	if adapted.Type != "function" || adapted.Function.Name != "head" {
		t.Fatalf("unexpected envelope %+v", adapted)
	}
	if adapted.Function.Parameters["additionalProperties"] != false {
		t.Fatal("additionalProperties restriction was lost")
	}
	properties := adapted.Function.Parameters["properties"].(map[string]any)
	lines := properties["lines"].(map[string]any)
	if lines["maximum"].(float64) != 200 || lines["minimum"].(float64) != 1 {
		t.Fatalf("numeric bounds were lost: %+v", lines)
	}
	if _, err := AdaptTool(mcpproto.Tool{Name: "bad", InputSchema: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("expected an invalid schema to be rejected")
	}
}

func TestPruneMessagesBoundsHistory(t *testing.T) {
	system := llm.Message{Role: "system", Content: SystemPrompt}
	user := llm.Message{Role: "user", Content: "Read README.md"}
	oversized := llm.Message{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("x", 32<<10)}
	messages := []llm.Message{
		system,
		{Role: "user", Content: "an older request"},
		{Role: "assistant", Content: "an older answer"},
		user,
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("c1", "cat", `{"path":"README.md"}`)}},
		oversized,
	}
	pruned, err := pruneMessages(messages, nil)
	if err != nil {
		t.Fatalf("pruneMessages failed: %v", err)
	}
	if pruned[0].Role != "system" || pruned[1].Content != user.Content {
		t.Fatalf("system prompt and current request must be retained: %+v", pruned[:2])
	}
	for _, message := range pruned {
		if message.Content == "an older answer" {
			t.Fatal("redundant earlier assistant text must be dropped")
		}
		if message.Role == "tool" && len(message.Content) > maxToolResultBytes+32 {
			t.Fatalf("oversized tool result was not truncated: %d bytes", len(message.Content))
		}
	}
	if _, err := pruneMessages(nil, nil); err == nil {
		t.Fatal("expected an empty conversation to be rejected")
	}
	huge := []llm.Message{system, {Role: "user", Content: strings.Repeat("x", 64<<10)}}
	if _, err := pruneMessages(huge, nil); err == nil {
		t.Fatal("expected an oversized request to be rejected")
	}
}

func TestConfigValidate(t *testing.T) {
	workspace := t.TempDir()
	base := Config{LlamaURL: "http://127.0.0.1:8080", Model: "local-phi-4-mini-instruct", MCPCommand: "./bin/coreutils-mcp", Workspace: workspace, Prompt: "hi"}
	valid := base
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected a valid configuration: %v", err)
	}

	missingURL := base
	missingURL.LlamaURL = "127.0.0.1:8080"
	if err := missingURL.Validate(); err == nil {
		t.Fatal("expected a non-http URL to be rejected")
	}
	missingPrompt := base
	missingPrompt.Prompt = "  "
	if err := missingPrompt.Validate(); err == nil {
		t.Fatal("expected a missing prompt to be rejected")
	}
	missingWorkspace := base
	missingWorkspace.Workspace = filepath.Join(workspace, "missing")
	if err := missingWorkspace.Validate(); err == nil {
		t.Fatal("expected a missing workspace to be rejected")
	}
	missingCommand := base
	missingCommand.MCPCommand = ""
	if err := missingCommand.Validate(); err == nil {
		t.Fatal("expected a missing MCP command to be rejected")
	}
}
