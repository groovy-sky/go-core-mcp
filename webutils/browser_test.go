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

func TestResolveChromeExecutableRejectsNonExecutableDefault(t *testing.T) {
	t.Parallel()

	nonExecutable := filepath.Join(t.TempDir(), "non-executable-default")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write non-executable file: %v", err)
	}

	_, err := resolveChromeExecutable(
		func(string) (string, bool) { return "", false },
		os.Stat,
		nonExecutable,
	)
	if err == nil {
		t.Fatal("expected error for non-executable default path")
	}
	if !strings.Contains(err.Error(), "path is not executable") {
		t.Fatalf("expected non-executable error, got %q", err)
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
