package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/groovy-sky/groovy-agent/coreutils"
	"github.com/groovy-sky/groovy-agent/internal/jsonschema"
	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

// maxHashBytes bounds sha256sum so hashing always fits the execution budget.
const maxHashBytes = 1 << 20

// maxCopyBytes bounds copy operations so a single request cannot exhaust the
// server's time or memory budget.
const maxCopyBytes = 1 << 20

// WriteCapableTools remain intentionally unavailable because they exceed the
// narrowly scoped filesystem operations implemented by this server.
var WriteCapableTools = []string{"link", "tee", "unlink"}

func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		names := make([]any, 0, len(required))
		for _, name := range required {
			names = append(names, name)
		}
		schema["required"] = names
	}
	return schema
}

func stringField(description string, maxLength int) map[string]any {
	return map[string]any{"type": "string", "description": description, "maxLength": maxLength}
}

func boolField(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func intField(description string, minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "minimum": minimum, "maximum": maximum}
}

func pathField() map[string]any {
	return stringField("Workspace-relative path.", 512)
}

func definitions() []tool {
	return []tool{
		{
			name:        "pwd",
			description: "Return the logical workspace path. This does not inspect the host filesystem.",
			schema:      object(map[string]any{}),
			run:         runPwd,
		},
		{
			name:        "ls",
			description: "List entries in a workspace directory.",
			schema: object(map[string]any{
				"path": pathField(),
			}),
			run: runLS,
		},
		{
			name:        "cat",
			description: "Read a bounded workspace text file.",
			schema: object(map[string]any{
				"path":      pathField(),
				"max_bytes": intField("Maximum bytes to read.", 1, 12<<10),
			}, "path"),
			run: runCat,
		},
		{
			name:        "head",
			description: "Read leading lines from a workspace file.",
			schema: object(map[string]any{
				"path": pathField(), "lines": intField("Number of lines.", 1, 200),
			}, "path"),
			run: runHead,
		},
		{
			name:        "tail",
			description: "Read trailing lines from a workspace file.",
			schema: object(map[string]any{
				"path": pathField(), "lines": intField("Number of lines.", 1, 200),
			}, "path"),
			run: runTail,
		},
		{
			name:        "grep",
			description: "Search a workspace text file for a pattern.",
			schema: object(map[string]any{
				"path": pathField(), "pattern": stringField("Pattern to search for.", 256),
				"ignore_case": boolField("Case-insensitive search."), "fixed": boolField("Treat the pattern as literal text."),
				"max_matches": intField("Maximum matches.", 1, 20),
			}, "path", "pattern"),
			run: runGrep,
		},
		{
			name:        "touch",
			description: "Create an empty workspace file or update its modification time.",
			schema:      object(map[string]any{"path": pathField()}, "path"),
			run:         runTouch,
		},
		{
			name:        "mkdir",
			description: "Create one workspace directory; parent directories must already exist.",
			schema:      object(map[string]any{"path": pathField()}, "path"),
			run:         runMkdir,
		},
		{
			name:        "cp",
			description: "Copy a bounded regular file within the workspace.",
			schema: object(map[string]any{
				"source": pathField(), "destination": pathField(), "overwrite": boolField("Replace an existing regular destination file."),
			}, "source", "destination"),
			run: runCopy,
		},
		{
			name:        "mv",
			description: "Move or rename a file or empty directory within the workspace.",
			schema: object(map[string]any{
				"source": pathField(), "destination": pathField(), "overwrite": boolField("Replace an existing destination."),
			}, "source", "destination"),
			run: runMove,
		},
		{
			name:        "rm",
			description: "Remove one regular file or symbolic link in the workspace. Directories are not removed.",
			schema:      object(map[string]any{"path": pathField()}, "path"),
			run:         runRemove,
		},
		{
			name:        "rmdir",
			description: "Remove one empty workspace directory. Recursive deletion is not supported.",
			schema:      object(map[string]any{"path": pathField()}, "path"),
			run:         runRemoveDirectory,
		},
		{
			name:        "coreutils_run",
			description: "Run an approved, read-only text utility on supplied stdin. Shell syntax and file paths are not supported.",
			schema: object(map[string]any{
				"command": stringField("Name of an approved core utility.", 32),
				"args":    map[string]any{"type": "array", "description": "Validated utility arguments; shell syntax is not supported.", "items": stringField("Argument.", 4096), "maxItems": 32},
				"stdin":   stringField("Optional UTF-8 text supplied to standard input.", 64<<10),
			}, "command"),
			run: runCoreutils,
		},
	}
}

func runCoreutils(ctx context.Context, _ *Server, arguments map[string]any) (payload, error) {
	name, err := requireString(arguments, "command")
	if err != nil {
		return payload{}, err
	}
	command, ok := coreutils.LookupCommand(name)
	if !ok || !command.ExposeToMCP || !command.ReadOnly {
		return payload{}, fail(mcpproto.ErrorPermissionDenied, "command %q is not permitted", name)
	}
	args := []string{}
	if rawArgs, ok := arguments["args"].([]any); ok {
		for _, raw := range rawArgs {
			argument, ok := raw.(string)
			if !ok {
				return payload{}, fail(mcpproto.ErrorInvalidArguments, "args must contain only strings")
			}
			args = append(args, argument)
		}
	}
	if err := command.ValidateArgs(args); err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "invalid arguments: %s", err)
	}
	stdin, _ := arguments["stdin"].(string)
	var stdout, stderr bytes.Buffer
	if err := command.Run(ctx, args, bytes.NewBufferString(stdin), &stdout, &stderr); err != nil {
		return payload{}, err
	}
	output, truncated := clampResult(stdout.String(), 256<<10)
	return payload{Truncated: truncated, Result: map[string]any{"success": true, "command": name, "stdout": output, "stderr": stderr.String(), "truncated": truncated}}, nil
}

func optionalBool(arguments map[string]any, key string) bool {
	value, _ := arguments[key].(bool)
	return value
}

func optionalInt(arguments map[string]any, key string, fallback int) int {
	value, ok := arguments[key]
	if !ok {
		return fallback
	}
	number, ok := jsonschema.Number(value)
	if !ok {
		return fallback
	}
	return number
}

func requireString(arguments map[string]any, key string) (string, error) {
	value, ok := arguments[key].(string)
	if !ok {
		return "", fail(mcpproto.ErrorInvalidArguments, "%q must be a string", key)
	}
	return value, nil
}

func runPwd(_ context.Context, s *Server, _ map[string]any) (payload, error) {
	logical := "/" + filepath.Base(s.workspace)
	return payload{
		Output:   logical,
		Metadata: map[string]any{"workspace": logical, "relative_root": "."},
	}, nil
}

func runLS(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	relative, _ := arguments["path"].(string)
	if relative == "" {
		relative = "."
	}
	path, err := s.resolvePath(relative)
	if err != nil {
		return payload{}, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "directory could not be listed")
	}
	if len(entries) > 200 {
		entries = entries[:200]
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return payload{Output: coreutils.JoinLines(names), Truncated: len(entries) == 200}, nil
}

func runTouch(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	relative, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	path, err := s.resolveTouchPath(relative)
	if err != nil {
		return payload{}, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorPermissionDenied, "file could not be touched")
	}
	if err := file.Close(); err != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "file could not be touched")
	}
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "file timestamp could not be updated")
	}
	return payload{Output: relative, Metadata: map[string]any{"path": relative}}, nil
}

func runMkdir(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	relative, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	path, err := s.resolveTouchPath(relative)
	if err != nil {
		return payload{}, err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		if os.IsExist(err) {
			return payload{}, fail(mcpproto.ErrorInvalidArguments, "path already exists")
		}
		return payload{}, fail(mcpproto.ErrorToolError, "directory could not be created")
	}
	return payload{Output: relative, Metadata: map[string]any{"path": relative}}, nil
}

func runCopy(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	source, err := requireString(arguments, "source")
	if err != nil {
		return payload{}, err
	}
	destination, err := requireString(arguments, "destination")
	if err != nil {
		return payload{}, err
	}
	sourcePath, err := s.resolvePath(source)
	if err != nil {
		return payload{}, err
	}
	info, err := os.Stat(sourcePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxCopyBytes {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "source must be a regular file no larger than 1 MiB")
	}
	destinationPath, err := s.resolveTouchPath(destination)
	if err != nil {
		return payload{}, err
	}
	if sourcePath == destinationPath {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "source and destination must differ")
	}
	if _, err := os.Stat(destinationPath); err == nil && !optionalBool(arguments, "overwrite") {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "destination already exists; set overwrite to replace it")
	}
	input, err := os.Open(sourcePath)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "source could not be read")
	}
	defer input.Close()
	output, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "destination could not be written")
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "file could not be copied")
	}
	return payload{Output: destination, Metadata: map[string]any{"source": source, "destination": destination}}, nil
}

func runMove(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	source, err := requireString(arguments, "source")
	if err != nil {
		return payload{}, err
	}
	destination, err := requireString(arguments, "destination")
	if err != nil {
		return payload{}, err
	}
	sourcePath, err := s.resolvePath(source)
	if err != nil || sourcePath == s.workspace {
		return payload{}, fail(mcpproto.ErrorWorkspaceViolation, "source path is not allowed")
	}
	destinationPath, err := s.resolveTouchPath(destination)
	if err != nil {
		return payload{}, err
	}
	if _, err := os.Lstat(destinationPath); err == nil && !optionalBool(arguments, "overwrite") {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "destination already exists; set overwrite to replace it")
	}
	if err := os.Rename(sourcePath, destinationPath); err != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "path could not be moved")
	}
	return payload{Output: destination, Metadata: map[string]any{"source": source, "destination": destination}}, nil
}

func runRemove(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	relative, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	path, err := s.resolvePath(relative)
	if err != nil || path == s.workspace {
		return payload{}, fail(mcpproto.ErrorWorkspaceViolation, "path is not allowed")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "path is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "file could not be removed")
	}
	return payload{Output: relative, Metadata: map[string]any{"path": relative}}, nil
}

func runRemoveDirectory(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	relative, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	path, err := s.resolvePath(relative)
	if err != nil || path == s.workspace {
		return payload{}, fail(mcpproto.ErrorWorkspaceViolation, "path is not allowed")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "path is not a directory")
	}
	if err := os.Remove(path); err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "directory must be empty")
	}
	return payload{Output: relative, Metadata: map[string]any{"path": relative}}, nil
}

func runDate(_ context.Context, _ *Server, arguments map[string]any) (payload, error) {
	now := time.Now()
	if optionalBool(arguments, "utc") {
		now = now.UTC()
	}
	return payload{Output: now.Format(time.RFC3339), Metadata: map[string]any{"format": "RFC3339"}}, nil
}

func runCat(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	limit := optionalInt(arguments, "max_bytes", s.limits.MaxFileReadBytes)
	content, truncated, err := s.readFile(path, limit)
	if err != nil {
		return payload{}, err
	}
	lines, clamped := coreutils.ClampLines(coreutils.SplitLines(content))
	output, cut := coreutils.Clamp(coreutils.JoinLines(lines), limit)
	return payload{
		Output:    output,
		Truncated: truncated || clamped || cut,
		Metadata:  map[string]any{"path": path, "bytes": len(content)},
	}, nil
}

func runHead(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	count := optionalInt(arguments, "lines", 20)
	content, truncated, err := s.readFile(path, s.limits.MaxFileReadBytes)
	if err != nil {
		return payload{}, err
	}
	output, cut := coreutils.Head(content, count)
	return payload{
		Output:    output,
		Truncated: truncated || cut,
		Metadata:  map[string]any{"path": path, "lines": count},
	}, nil
}

func runTail(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	count := optionalInt(arguments, "lines", 20)
	content, truncated, err := s.readFile(path, s.limits.MaxFileReadBytes)
	if err != nil {
		return payload{}, err
	}
	output, cut := coreutils.Tail(content, count)
	return payload{
		Output:    output,
		Truncated: truncated || cut,
		Metadata:  map[string]any{"path": path, "lines": count},
	}, nil
}

func runWC(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	_, hasPath := arguments["path"]
	_, hasText := arguments["text"]
	if hasPath == hasText {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "provide exactly one of \"path\" or \"text\"")
	}
	content := ""
	truncated := false
	if hasPath {
		path, err := requireString(arguments, "path")
		if err != nil {
			return payload{}, err
		}
		content, truncated, err = s.readFile(path, s.limits.MaxFileReadBytes)
		if err != nil {
			return payload{}, err
		}
	} else {
		text, err := s.requireText(arguments, "text")
		if err != nil {
			return payload{}, err
		}
		content = text
	}
	counts := coreutils.WordCount(content)
	return payload{
		Output:    fmt.Sprintf("%d lines %d words %d bytes", counts.Lines, counts.Words, counts.Bytes),
		Truncated: truncated,
		Metadata:  map[string]any{"lines": counts.Lines, "words": counts.Words, "bytes": counts.Bytes},
	}, nil
}

func runGrep(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	pattern, err := requireString(arguments, "pattern")
	if err != nil {
		return payload{}, err
	}
	maxMatches := optionalInt(arguments, "max_matches", s.limits.MaxGrepMatches)
	if maxMatches > s.limits.MaxGrepMatches {
		maxMatches = s.limits.MaxGrepMatches
	}
	content, truncated, err := s.readFile(path, s.limits.MaxFileReadBytes)
	if err != nil {
		return payload{}, err
	}
	matches, cut, err := coreutils.Grep(content, coreutils.GrepOptions{
		Pattern:    pattern,
		IgnoreCase: optionalBool(arguments, "ignore_case"),
		FixedText:  optionalBool(arguments, "fixed"),
		MaxMatches: maxMatches,
	})
	if err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", err.Error())
	}
	lines := make([]string, 0, len(matches))
	for _, match := range matches {
		lines = append(lines, fmt.Sprintf("%d:%s", match.Line, match.Text))
	}
	return payload{
		Output:    coreutils.JoinLines(lines),
		Truncated: truncated || cut,
		Metadata:  map[string]any{"path": path, "matches": len(matches)},
	}, nil
}

func runSha256Sum(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	relative, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	path, err := s.resolvePath(relative)
	if err != nil {
		return payload{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorToolError, "file could not be inspected")
	}
	if !info.Mode().IsRegular() {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "path is not a regular file")
	}
	if info.Size() > maxHashBytes {
		return payload{}, fail(mcpproto.ErrorResultTooLarge, "file is too large to hash within the execution budget")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return payload{}, fail(mcpproto.ErrorPermissionDenied, "file is not readable")
		}
		return payload{}, fail(mcpproto.ErrorToolError, "file could not be read")
	}
	return payload{
		Output:   coreutils.Sha256Sum(data),
		Metadata: map[string]any{"path": relative, "bytes": len(data)},
	}, nil
}

func runBasename(_ context.Context, _ *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	suffix, _ := arguments["suffix"].(string)
	return payload{Output: coreutils.Basename(path, suffix)}, nil
}

func runDirname(_ context.Context, _ *Server, arguments map[string]any) (payload, error) {
	path, err := requireString(arguments, "path")
	if err != nil {
		return payload{}, err
	}
	return payload{Output: coreutils.Dirname(path)}, nil
}

func runBase64(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	if optionalBool(arguments, "decode") {
		decoded, decodeErr := coreutils.Base64Decode(text)
		if decodeErr != nil {
			return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", decodeErr.Error())
		}
		return payload{Output: decoded}, nil
	}
	return payload{Output: coreutils.Base64Encode(text)}, nil
}

func runCut(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	delimiter, err := requireString(arguments, "delimiter")
	if err != nil {
		return payload{}, err
	}
	rawFields, ok := arguments["fields"].([]any)
	if !ok {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "\"fields\" must be an array of integers")
	}
	fields := make([]int, 0, len(rawFields))
	for _, raw := range rawFields {
		field, ok := jsonschema.Number(raw)
		if !ok {
			return payload{}, fail(mcpproto.ErrorInvalidArguments, "\"fields\" must contain integers")
		}
		fields = append(fields, field)
	}
	output, err := coreutils.Cut(text, delimiter, fields)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", err.Error())
	}
	return payload{Output: output}, nil
}

func runPaste(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	rawInputs, ok := arguments["inputs"].([]any)
	if !ok {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "\"inputs\" must be an array of strings")
	}
	total := 0
	inputs := make([]string, 0, len(rawInputs))
	for _, raw := range rawInputs {
		text, ok := raw.(string)
		if !ok {
			return payload{}, fail(mcpproto.ErrorInvalidArguments, "\"inputs\" must contain strings")
		}
		total += len(text)
		if total > s.limits.MaxFileReadBytes {
			return payload{}, fail(mcpproto.ErrorInvalidArguments, "input text exceeds the allowed size")
		}
		inputs = append(inputs, text)
	}
	delimiter, _ := arguments["delimiter"].(string)
	output, err := coreutils.Paste(inputs, delimiter)
	if err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", err.Error())
	}
	return payload{Output: output}, nil
}

func runSort(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	output := coreutils.Sort(text,
		optionalBool(arguments, "reverse"),
		optionalBool(arguments, "numeric"),
		optionalBool(arguments, "unique"),
	)
	return payload{Output: output}, nil
}

func runTr(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	from, err := requireString(arguments, "from")
	if err != nil {
		return payload{}, err
	}
	to, _ := arguments["to"].(string)
	output, err := coreutils.Tr(text, from, to, optionalBool(arguments, "delete"))
	if err != nil {
		return payload{}, fail(mcpproto.ErrorInvalidArguments, "%s", err.Error())
	}
	return payload{Output: output}, nil
}

func runUniq(_ context.Context, s *Server, arguments map[string]any) (payload, error) {
	text, err := s.requireText(arguments, "text")
	if err != nil {
		return payload{}, err
	}
	return payload{Output: coreutils.Uniq(text, optionalBool(arguments, "count"))}, nil
}
