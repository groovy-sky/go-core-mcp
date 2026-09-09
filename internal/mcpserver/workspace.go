package mcpserver

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

// canonicalWorkspace validates and canonicalizes the workspace root.
func canonicalWorkspace(workspace string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", errors.New("workspace must not be empty")
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", errors.New("workspace path could not be resolved")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("workspace directory does not exist")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("workspace is not a directory")
	}
	return resolved, nil
}

// resolvePath maps a workspace-relative path to a canonical absolute path and
// rejects traversal, absolute paths, and symbolic-link escapes.
func (s *Server) resolvePath(relative string) (string, error) {
	if strings.TrimSpace(relative) == "" {
		return "", fail(mcpproto.ErrorInvalidArguments, "path must not be empty")
	}
	if strings.ContainsRune(relative, '\x00') {
		return "", fail(mcpproto.ErrorInvalidArguments, "path contains an invalid character")
	}
	if filepath.IsAbs(relative) || strings.HasPrefix(relative, "/") || strings.HasPrefix(relative, `\`) {
		return "", fail(mcpproto.ErrorWorkspaceViolation, "absolute paths are not allowed; use workspace-relative paths")
	}
	cleaned := filepath.Clean(relative)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fail(mcpproto.ErrorWorkspaceViolation, "path escapes the workspace")
	}
	joined := filepath.Clean(filepath.Join(s.workspace, cleaned))
	if !s.inside(joined) {
		return "", fail(mcpproto.ErrorWorkspaceViolation, "path escapes the workspace")
	}
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fail(mcpproto.ErrorToolError, "path does not exist")
		}
		if os.IsPermission(err) {
			return "", fail(mcpproto.ErrorPermissionDenied, "path is not accessible")
		}
		return "", fail(mcpproto.ErrorWorkspaceViolation, "path could not be resolved inside the workspace")
	}
	if !s.inside(resolved) {
		return "", fail(mcpproto.ErrorWorkspaceViolation, "path resolves outside the workspace")
	}
	return resolved, nil
}

// resolveTouchPath resolves an existing file or a new file beneath an existing
// workspace directory. It rejects the same traversal and symlink escapes as
// resolvePath before a write is attempted.
func (s *Server) resolveTouchPath(relative string) (string, error) {
	path, err := s.resolvePath(relative)
	if err == nil {
		return path, nil
	}
	var typed *toolError
	if !errors.As(err, &typed) || typed.Category != mcpproto.ErrorToolError || typed.Message != "path does not exist" {
		return "", err
	}
	if strings.TrimSpace(relative) == "" || strings.ContainsRune(relative, '\x00') || filepath.IsAbs(relative) {
		return "", fail(mcpproto.ErrorWorkspaceViolation, "path is not allowed")
	}
	cleaned := filepath.Clean(relative)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fail(mcpproto.ErrorWorkspaceViolation, "path escapes the workspace")
	}
	parent, err := s.resolvePath(filepath.Dir(cleaned))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return "", fail(mcpproto.ErrorInvalidArguments, "parent directory is not available")
	}
	path = filepath.Join(parent, filepath.Base(cleaned))
	if !s.inside(path) {
		return "", fail(mcpproto.ErrorWorkspaceViolation, "path escapes the workspace")
	}
	return path, nil
}

func (s *Server) inside(path string) bool {
	if path == s.workspace {
		return true
	}
	return strings.HasPrefix(path, s.workspace+string(filepath.Separator))
}

// relativePath renders a canonical path as a workspace-relative logical path.
func (s *Server) relativePath(path string) string {
	relative, err := filepath.Rel(s.workspace, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(relative)
}

// readFile reads at most limit bytes from a regular workspace file.
func (s *Server) readFile(relative string, limit int) (string, bool, error) {
	path, err := s.resolvePath(relative)
	if err != nil {
		return "", false, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", false, fail(mcpproto.ErrorToolError, "file could not be inspected")
	}
	if info.IsDir() {
		return "", false, fail(mcpproto.ErrorInvalidArguments, "path is a directory, not a file")
	}
	if !info.Mode().IsRegular() {
		return "", false, fail(mcpproto.ErrorInvalidArguments, "path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsPermission(err) {
			return "", false, fail(mcpproto.ErrorPermissionDenied, "file is not readable")
		}
		return "", false, fail(mcpproto.ErrorToolError, "file could not be opened")
	}
	defer file.Close()

	if limit <= 0 || limit > s.limits.MaxFileReadBytes {
		limit = s.limits.MaxFileReadBytes
	}
	buffer := make([]byte, limit+1)
	count, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false, fail(mcpproto.ErrorToolError, "file could not be read")
	}
	truncated := false
	if count > limit {
		count = limit
		truncated = true
	}
	return string(buffer[:count]), truncated, nil
}

// requireText extracts a bounded text argument.
func (s *Server) requireText(arguments map[string]any, key string) (string, error) {
	value, ok := arguments[key].(string)
	if !ok {
		return "", fail(mcpproto.ErrorInvalidArguments, "%q must be a string", key)
	}
	if len(value) > s.limits.MaxFileReadBytes {
		return "", fail(mcpproto.ErrorInvalidArguments, "input text exceeds the allowed size")
	}
	return value, nil
}

// readTextFile reads one regular workspace file, rejects binary payloads, and
// reports actionable errors for oversize or unreadable input.
func (s *Server) readTextFile(relative string, limit int) (string, error) {
	path, err := s.resolvePath(relative)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fail(mcpproto.ErrorToolError, "file could not be inspected")
	}
	if info.IsDir() {
		return "", fail(mcpproto.ErrorInvalidArguments, "path is a directory, not a file")
	}
	if !info.Mode().IsRegular() {
		return "", fail(mcpproto.ErrorInvalidArguments, "path is not a regular file")
	}
	if limit <= 0 || limit > s.limits.MaxFileReadBytes {
		limit = s.limits.MaxFileReadBytes
	}
	if info.Size() > int64(limit) {
		return "", fail(mcpproto.ErrorResultTooLarge, "file exceeds the %d-byte read limit; use a smaller file or increase constraints", limit)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsPermission(err) {
			return "", fail(mcpproto.ErrorPermissionDenied, "file is not readable")
		}
		return "", fail(mcpproto.ErrorToolError, "file could not be read")
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
		return "", fail(mcpproto.ErrorInvalidArguments, "file appears to be binary; only UTF-8 text files are supported")
	}
	return string(data), nil
}
