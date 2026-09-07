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
	Tools: []string{"coreutils_run"},
}

// profiles are evaluated in this fixed order so selection is deterministic.
var profiles = []struct {
	profile  Profile
	keywords []string
}{
	{
		profile:  Profile{Name: "date", Tools: []string{"coreutils_run"}},
		keywords: []string{"date", "time", "clock", "timestamp", "today", "utc", "current time"},
	},
	{
		profile:  Profile{Name: "file_search", Tools: []string{"coreutils_run"}},
		keywords: []string{"grep", "search", "find occurrences", "occurrence", "occurrences", "matches", "matching", "pattern", "todo", "look for", "contains"},
	},
	{
		profile:  Profile{Name: "file_inspection", Tools: []string{"pwd", "coreutils_run"}},
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
