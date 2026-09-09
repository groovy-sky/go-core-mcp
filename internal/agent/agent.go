// Package agent implements the bounded agent loop that connects a local
// llama-server to the coreutils MCP server.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/llm"
	"github.com/groovy-sky/groovy-agent/internal/mcpclient"
	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

// Bounded defaults from PLAN.md.
const (
	MaxModelRounds    = 3
	MaxTotalToolCalls = 5
	MaxResultBytes    = 16 << 10
	MCPStartupTimeout = 10 * time.Second
	ContextTokens     = 4096
	ReservedTokens    = llm.MaxOutputTokens

	// RoundLimitMessage is emitted verbatim when the round budget is spent.
	RoundLimitMessage = "Agent stopped: maximum model rounds reached."
)

// SystemPrompt is the short prompt from PLAN.md.
const SystemPrompt = `You are a local assistant with access to coreutils tools.

Use tools when workspace data or an exact calculation is required.
Never invent tool results.
Call only listed tools.
Use JSON arguments matching the tool schema.
After receiving results, answer concisely.
Do not repeat large tool output unless requested.`

// AllowedTools is the default bounded workspace/coreutils policy.
var AllowedTools = []string{
	"cat", "coreutils_run", "cp", "find", "grep", "grep_file", "grep_text", "head", "ls", "mkdir", "mv", "pwd", "read_file", "rm", "rmdir", "tail", "touch", "write_file",
}

// Config holds the validated CLI configuration.
type Config struct {
	LlamaURL   string
	Model      string
	MCPCommand string
	MCPArgs    []string
	Workspace  string
	Prompt     string
}

// Validate checks the configuration and canonicalizes the workspace.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.LlamaURL) == "" {
		return errors.New("--llama-url is required")
	}
	if !strings.HasPrefix(c.LlamaURL, "http://") && !strings.HasPrefix(c.LlamaURL, "https://") {
		return errors.New("--llama-url must be an http or https URL")
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("--model is required")
	}
	if strings.TrimSpace(c.MCPCommand) == "" {
		return errors.New("--mcp-command is required")
	}
	if strings.TrimSpace(c.Prompt) == "" {
		return errors.New("a prompt argument is required")
	}
	if strings.TrimSpace(c.Workspace) == "" {
		c.Workspace = "."
	}
	workspace, err := filepath.Abs(c.Workspace)
	if err != nil {
		return errors.New("--workspace could not be resolved")
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return fmt.Errorf("--workspace is not usable: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return errors.New("--workspace must be an existing directory")
	}
	c.Workspace = workspace
	return nil
}

// Session owns the model client, the MCP session, and the discovered tools.
type Session struct {
	config     Config
	model      modelClient
	mcp        toolClient
	discovered map[string]mcpproto.Tool
	logger     *log.Logger
	out        io.Writer
	toolCalls  int
}

// modelClient is the subset of the LLM client used by the loop.
type modelClient interface {
	Complete(ctx context.Context, messages []llm.Message, tools []llm.Tool) (llm.Message, error)
}

// toolClient is the subset of the MCP client used by the loop.
type toolClient interface {
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (mcpproto.CallToolResult, error)
}

// Run performs the full lifecycle: validate, connect, discover, loop, clean up.
func Run(ctx context.Context, config Config, stdout io.Writer, stderr io.Writer) error {
	logger := log.New(stderr, "agent: ", 0)

	if err := config.Validate(); err != nil {
		return err
	}
	logger.Printf("workspace=%s model=%s", config.Workspace, config.Model)

	model := llm.New(config.LlamaURL, config.Model)
	if err := model.Ping(ctx); err != nil {
		return err
	}
	logger.Printf("llama-server reachable at %s", config.LlamaURL)

	mcpCtx, cancelMCP := context.WithCancel(ctx)
	defer cancelMCP()

	args := append([]string{}, config.MCPArgs...)
	args = append(args, "--workspace", config.Workspace)
	client, err := mcpclient.StartProcess(mcpCtx, config.MCPCommand, args, config.Workspace, stderr)
	if err != nil {
		return err
	}
	defer client.Close()

	startupCtx, cancelStartup := context.WithTimeout(ctx, MCPStartupTimeout)
	defer cancelStartup()
	info, err := client.Initialize(startupCtx)
	if err != nil {
		return fmt.Errorf("MCP session could not be established: %w", err)
	}
	logger.Printf("MCP server %s %s ready", info.ServerInfo.Name, info.ServerInfo.Version)

	tools, err := client.ListTools(startupCtx)
	if err != nil {
		return fmt.Errorf("tool discovery failed: %w", err)
	}
	discovered := FilterDiscovered(tools, logger)
	if len(discovered) == 0 {
		return errors.New("no allowed tools were discovered")
	}

	session := &Session{
		config:     config,
		model:      model,
		mcp:        client,
		discovered: discovered,
		logger:     logger,
		out:        stdout,
	}
	return session.Loop(ctx)
}

// FilterDiscovered keeps only tools allowed by the read-only policy. Unexpected
// tools are logged and denied.
func FilterDiscovered(tools []mcpproto.Tool, logger *log.Logger) map[string]mcpproto.Tool {
	allowed := make(map[string]struct{}, len(AllowedTools))
	for _, name := range AllowedTools {
		allowed[name] = struct{}{}
	}
	kept := make(map[string]mcpproto.Tool, len(tools))
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if _, ok := allowed[tool.Name]; !ok {
			if logger != nil {
				logger.Printf("denied unexpected tool %q advertised by the MCP server", tool.Name)
			}
			continue
		}
		kept[tool.Name] = tool
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if logger != nil {
		logger.Printf("discovered tools: %s", strings.Join(names, ", "))
		for _, name := range AllowedTools {
			if _, ok := kept[name]; !ok {
				logger.Printf("warning: allowed tool %q was not advertised", name)
			}
		}
	}
	return kept
}

// Loop runs the bounded agent loop.
func (s *Session) Loop(ctx context.Context) error {
	profile := SelectProfile(s.config.Prompt)
	exposed, tools := s.exposeProfile(profile)
	if len(exposed) == 0 {
		return fmt.Errorf("profile %q has no usable tools", profile.Name)
	}
	s.logger.Printf("profile=%s exposed=%s", profile.Name, strings.Join(toolNames(tools), ", "))

	messages := []llm.Message{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: s.config.Prompt},
	}

	for round := 1; round <= MaxModelRounds; round++ {
		s.logger.Printf("model round %d/%d", round, MaxModelRounds)
		pruned, err := pruneMessages(messages, tools)
		if err != nil {
			return err
		}
		reply, err := s.model.Complete(ctx, pruned, tools)
		if err != nil {
			return err
		}
		messages = append(messages, reply)

		if len(reply.ToolCalls) == 0 {
			fmt.Fprintln(s.out, strings.TrimSpace(reply.Content))
			return nil
		}

		for index, call := range reply.ToolCalls {
			message, err := s.runToolCall(ctx, exposed, call, round, index)
			if err != nil {
				return err
			}
			messages = append(messages, message)
		}
	}

	fmt.Fprintln(s.out, RoundLimitMessage)
	return nil
}

func (s *Session) exposeProfile(profile Profile) (map[string]mcpproto.Tool, []llm.Tool) {
	exposed := make(map[string]mcpproto.Tool, len(profile.Tools))
	tools := make([]llm.Tool, 0, len(profile.Tools))
	for _, name := range profile.Tools {
		if len(tools) >= MaxExposedTools {
			break
		}
		discovered, ok := s.discovered[name]
		if !ok {
			s.logger.Printf("warning: profile tool %q is unavailable", name)
			continue
		}
		adapted, err := AdaptTool(discovered)
		if err != nil {
			s.logger.Printf("warning: tool %q has an unusable schema", name)
			continue
		}
		exposed[name] = discovered
		tools = append(tools, adapted)
	}
	return exposed, tools
}

// AdaptTool converts an MCP tool definition to the model function-tool
// envelope, preserving the JSON-schema constraints.
func AdaptTool(tool mcpproto.Tool) (llm.Tool, error) {
	parameters := map[string]any{}
	if err := json.Unmarshal(tool.InputSchema, &parameters); err != nil {
		return llm.Tool{}, fmt.Errorf("tool %q has an invalid input schema", tool.Name)
	}
	return llm.Tool{
		Type: "function",
		Function: llm.Function{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  parameters,
		},
	}, nil
}

func toolNames(tools []llm.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Function.Name)
	}
	return names
}
