package coreutils

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Match is a single grep match.
type Match struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GrepOptions bounds a grep invocation.
type GrepOptions struct {
	Pattern    string
	IgnoreCase bool
	FixedText  bool
	MaxMatches int
}

// CompileMatcher validates the pattern options and returns a line matcher that
// can be reused across text and filesystem search tools.
func CompileMatcher(pattern string, ignoreCase, fixedText bool) (func(string) bool, error) {
	if pattern == "" {
		return nil, errors.New("pattern must not be empty")
	}
	if len(pattern) > 256 {
		return nil, errors.New("pattern is too long")
	}

	if fixedText {
		needle := pattern
		if ignoreCase {
			needle = strings.ToLower(needle)
			return func(line string) bool { return strings.Contains(strings.ToLower(line), needle) }, nil
		}
		return func(line string) bool { return strings.Contains(line, needle) }, nil
	}

	expression, err := regexp.Compile(func() string {
		if ignoreCase {
			return "(?i)" + pattern
		}
		return pattern
	}())
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %s", err)
	}
	expression.Longest()
	return expression.MatchString, nil
}

// Grep searches text line by line and stops after MaxMatches matches.
func Grep(text string, options GrepOptions) ([]Match, bool, error) {
	if options.MaxMatches <= 0 {
		options.MaxMatches = 20
	}

	matcher, err := CompileMatcher(options.Pattern, options.IgnoreCase, options.FixedText)
	if err != nil {
		return nil, false, err
	}

	matches := make([]Match, 0, options.MaxMatches)
	truncated := false
	for index, line := range SplitLines(text) {
		if !matcher(line) {
			continue
		}
		if len(matches) >= options.MaxMatches {
			truncated = true
			break
		}
		clamped, cut := ClampLine(line)
		truncated = truncated || cut
		matches = append(matches, Match{Line: index + 1, Text: clamped})
	}
	return matches, truncated, nil
}
