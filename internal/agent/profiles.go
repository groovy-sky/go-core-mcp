package agent

import "strings"

// Profile is a deterministic, bounded set of tools exposed for one request.
type Profile struct {
	Name  string
	Tools []string
}

// MaxExposedTools is the per-request tool limit from PLAN.md.
const MaxExposedTools = 6

// FallbackProfile is used when no profile matches confidently.
var FallbackProfile = Profile{
	Name:  "fallback",
	Tools: []string{"pwd", "ls", "cat", "grep", "find", "coreutils_run"},
}

// profiles are evaluated in this fixed order so selection is deterministic.
var profiles = []struct {
	profile  Profile
	keywords []string
}{
	{
		profile:  Profile{Name: "web_browse", Tools: []string{"search_web", "browse_url"}},
		keywords: []string{"browse", "web", "website", "url", "https://", "http://", "page", "search web", "search online", "current information", "latest"},
	},
	{
		profile:  Profile{Name: "date", Tools: []string{"coreutils_run"}},
		keywords: []string{"date", "time", "clock", "timestamp", "today", "utc", "current time"},
	},
	{
		profile:  Profile{Name: "text_search", Tools: []string{"grep"}},
		keywords: []string{"search text", "grep text", "supplied text", "input text", "string search", "search this text", "matching lines"},
	},
	{
		profile:  Profile{Name: "file_search", Tools: []string{"find", "grep", "ls", "head", "cat"}},
		keywords: []string{"grep", "search", "find occurrences", "occurrence", "occurrences", "matches", "matching", "pattern", "todo", "look for", "contains"},
	},
	{
		profile:  Profile{Name: "filesystem_find", Tools: []string{"find", "ls", "cat", "grep"}},
		keywords: []string{"find file", "find files", "find directory", "find directories", "locate", "recursive", "under", "walk"},
	},
	{
		profile:  Profile{Name: "file_inspection", Tools: []string{"pwd", "ls", "cat", "head", "tail", "coreutils_run"}},
		keywords: []string{"read", "show", "workspace", "readme", "file", "beginning", "summarize", "summary", "content", "contents", "checksum", "sha256", "hash", "inspect", "lines of", "print"},
	},
	{
		profile:  Profile{Name: "path_processing", Tools: []string{"coreutils_run"}},
		keywords: []string{"basename", "dirname", "base name", "directory name", "path component", "strip the directory", "file extension"},
	},
	{
		profile:  Profile{Name: "text_processing", Tools: []string{"coreutils_run"}},
		keywords: []string{"sort", "unique", "uniq", "duplicate", "base64", "encode", "decode", "translate characters", "fields", "column", "columns", "cut", "merge lines"},
	},
	{
		profile:  Profile{Name: "file_write", Tools: []string{"ls", "cat", "write_file", "mkdir", "touch"}},
		keywords: []string{"write", "append", "overwrite", "save", "write to", "update file", "write file", "edit file"},
	},
	{
		profile:  Profile{Name: "file_management", Tools: []string{"ls", "mkdir", "touch", "write_file", "cp", "mv"}},
		keywords: []string{"touch", "create", "new", "empty", "mkdir", "directory", "copy", "move", "rename", "create file", "new file", "empty file"},
	},
	{
		profile:  Profile{Name: "file_cleanup", Tools: []string{"ls", "rm", "rmdir"}},
		keywords: []string{"remove", "delete"},
	},
}

// SelectProfile deterministically chooses the smallest relevant tool profile
// for a request. It never calls the model.
func SelectProfile(request string) Profile {
	lowered := strings.ToLower(request)
	best := Profile{}
	bestScore := 0
	for _, candidate := range profiles {
		score := 0
		for _, keyword := range candidate.keywords {
			if strings.Contains(lowered, keyword) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = candidate.profile, score
		}
	}
	if bestScore == 0 {
		return FallbackProfile
	}
	if len(best.Tools) > MaxExposedTools {
		best.Tools = best.Tools[:MaxExposedTools]
	}
	return best
}
