package webutils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveChromeExecutableUsesConfiguredOverride(t *testing.T) {
	t.Parallel()

	defaultPath := makeExecutable(t, "default-chromium")
	overridePath := makeExecutable(t, "override-chrome")

	path, err := resolveChromeExecutable(
		func(key string) (string, bool) {
			if key != chromeExecutableEnvVar {
				return "", false
			}
			return overridePath, true
		},
		os.Stat,
		defaultPath,
	)
	if err != nil {
		t.Fatalf("resolveChromeExecutable returned error: %v", err)
	}
	if path != overridePath {
		t.Fatalf("expected override path %q, got %q", overridePath, path)
	}
}

func TestResolveChromeExecutableUsesDefaultWhenOverrideUnset(t *testing.T) {
	t.Parallel()

	defaultPath := makeExecutable(t, "default-chromium")

	path, err := resolveChromeExecutable(
		func(string) (string, bool) { return "", false },
		os.Stat,
		defaultPath,
	)
	if err != nil {
		t.Fatalf("resolveChromeExecutable returned error: %v", err)
	}
	if path != defaultPath {
		t.Fatalf("expected default path %q, got %q", defaultPath, path)
	}
}

func TestResolveChromeExecutableTreatsBlankOverrideAsUnset(t *testing.T) {
	t.Parallel()

	defaultPath := makeExecutable(t, "default-chromium")

	path, err := resolveChromeExecutable(
		func(key string) (string, bool) {
			if key != chromeExecutableEnvVar {
				return "", false
			}
			return " \t ", true
		},
		os.Stat,
		defaultPath,
	)
	if err != nil {
		t.Fatalf("resolveChromeExecutable returned error: %v", err)
	}
	if path != defaultPath {
		t.Fatalf("expected default path %q, got %q", defaultPath, path)
	}
}

func TestResolveChromeExecutableRejectsMissingOverride(t *testing.T) {
	t.Parallel()

	defaultPath := makeExecutable(t, "default-chromium")
	missingPath := filepath.Join(t.TempDir(), "missing-chrome")

	_, err := resolveChromeExecutable(
		func(key string) (string, bool) {
			if key != chromeExecutableEnvVar {
				return "", false
			}
			return missingPath, true
		},
		os.Stat,
		defaultPath,
	)
	if err == nil {
		t.Fatal("expected error for missing override path")
	}
	message := err.Error()
	for _, needle := range []string{chromeExecutableEnvVar, missingPath, "unset " + chromeExecutableEnvVar, "Snap-wrapper chromium-browser"} {
		if !strings.Contains(message, needle) {
			t.Fatalf("expected error to contain %q, got %q", needle, message)
		}
	}
}

func TestResolveChromeExecutableRejectsMissingDefault(t *testing.T) {
	t.Parallel()

	missingDefault := filepath.Join(t.TempDir(), "missing-chromium")

	_, err := resolveChromeExecutable(
		func(string) (string, bool) { return "", false },
		os.Stat,
		missingDefault,
	)
	if err == nil {
		t.Fatal("expected error for missing default path")
	}
	message := err.Error()
	for _, needle := range []string{missingDefault, chromeExecutableEnvVar, "Snap-wrapper chromium-browser"} {
		if !strings.Contains(message, needle) {
			t.Fatalf("expected error to contain %q, got %q", needle, message)
		}
	}
}

func TestResolveChromeExecutableRejectsNonExecutableOverride(t *testing.T) {
	t.Parallel()

	defaultPath := makeExecutable(t, "default-chromium")
	nonExecutable := filepath.Join(t.TempDir(), "non-executable")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write non-executable file: %v", err)
	}

	_, err := resolveChromeExecutable(
		func(key string) (string, bool) {
			if key != chromeExecutableEnvVar {
				return "", false
			}
			return nonExecutable, true
		},
		os.Stat,
		defaultPath,
	)
	if err == nil {
		t.Fatal("expected error for non-executable override path")
	}
	if !strings.Contains(err.Error(), "path is not executable") {
		t.Fatalf("expected non-executable error, got %q", err)
	}
}

func TestResolveChromeArgsUsesConfiguredFlags(t *testing.T) {
	t.Parallel()

	flags, err := parseChromeArgs("--no-sandbox --disable-dev-shm-usage --proxy-server=https://example.com:443")
	if err != nil {
		t.Fatalf("parseChromeArgs returned error: %v", err)
	}
	if len(flags) != 3 {
		t.Fatalf("expected 3 flags, got %d", len(flags))
	}

	if got := flags[0]; got != (chromeFlag{name: "no-sandbox"}) {
		t.Fatalf("unexpected first flag: %#v", got)
	}
	if got := flags[1]; got != (chromeFlag{name: "disable-dev-shm-usage"}) {
		t.Fatalf("unexpected second flag: %#v", got)
	}
	if got := flags[2]; got != (chromeFlag{name: "proxy-server", value: "https://example.com:443", hasValue: true}) {
		t.Fatalf("unexpected third flag: %#v", got)
	}
}

func TestResolveChromeArgsTreatsUnsetAndBlankAsEmpty(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		lookupEnv func(string) (string, bool)
	}{
		{
			name: "unset",
			lookupEnv: func(string) (string, bool) { return "", false },
		},
		{
			name: "blank",
			lookupEnv: func(key string) (string, bool) {
				if key != chromeArgsEnvVar {
					return "", false
				}
				return " \t ", true
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options, err := resolveChromeArgsFromEnv(tc.lookupEnv)
			if err != nil {
				t.Fatalf("resolveChromeArgsFromEnv returned error: %v", err)
			}
			if len(options) != 0 {
				t.Fatalf("expected no options, got %d", len(options))
			}
		})
	}
}

func TestResolveChromeArgsRejectsUnsupportedSyntax(t *testing.T) {
	t.Parallel()

	_, err := resolveChromeArgsFromEnv(func(key string) (string, bool) {
		if key != chromeArgsEnvVar {
			return "", false
		}
		return "no-sandbox", true
	})
	if err == nil {
		t.Fatal("expected error for unsupported Chromium argument syntax")
	}
	if !strings.Contains(err.Error(), chromeArgsEnvVar) {
		t.Fatalf("expected error to mention %s, got %q", chromeArgsEnvVar, err)
	}
}

func makeExecutable(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable file: %v", err)
	}
	return path
}
