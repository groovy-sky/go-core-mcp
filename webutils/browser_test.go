package webutils

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
)

func TestConfiguredChromiumExecutableTrimsWhitespace(t *testing.T) {
	t.Setenv(chromeExecutableEnvVar, "  /usr/bin/chromium  \n")

	got := configuredChromiumExecutable(func(key string) string {
		if key != chromeExecutableEnvVar {
			t.Fatalf("unexpected env lookup: %q", key)
		}
		return "  /usr/bin/chromium  \n"
	})

	if got != "/usr/bin/chromium" {
		t.Fatalf("expected trimmed executable path, got %q", got)
	}
}

func TestDiscoverChromiumExecutableUsesChromedpSearchOrder(t *testing.T) {
	var lookedUp []string
	got, err := discoverChromiumExecutable(func(candidate string) (string, error) {
		lookedUp = append(lookedUp, candidate)
		if candidate == "chromium" {
			return "/usr/bin/chromium", nil
		}
		return "", exec.ErrNotFound
	})
	if err != nil {
		t.Fatalf("discoverChromiumExecutable returned error: %v", err)
	}
	if got != "/usr/bin/chromium" {
		t.Fatalf("expected chromium path, got %q", got)
	}
	if len(lookedUp) < 3 || lookedUp[0] != "headless_shell" || lookedUp[1] != "headless-shell" || lookedUp[2] != "chromium" {
		t.Fatalf("unexpected lookup order: %#v", lookedUp)
	}
}

func TestValidateChromiumExecutableRejectsMissingConfiguredPath(t *testing.T) {
	err := validateChromiumExecutable(context.Background(), "/missing/chrome", func(candidate string) (string, error) {
		if candidate != "/missing/chrome" {
			t.Fatalf("unexpected executable lookup: %q", candidate)
		}
		return "", exec.ErrNotFound
	}, func(context.Context, string) error {
		t.Fatal("probe should not run when lookup fails")
		return nil
	})
	if err == nil {
		t.Fatal("expected error for missing configured executable")
	}
	if !strings.Contains(err.Error(), chromeExecutableEnvVar) || !strings.Contains(err.Error(), "/missing/chrome") {
		t.Fatalf("expected actionable configured-path error, got %q", err)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("expected wrapped exec.ErrNotFound, got %v", err)
	}
}

func TestValidateChromiumExecutableRejectsBrokenAutoDiscoveredBrowser(t *testing.T) {
	probeErr := errors.New("snap launcher is present but chromium snap is not installed")
	err := validateChromiumExecutable(context.Background(), "", func(candidate string) (string, error) {
		if candidate == "chromium" {
			return "/snap/bin/chromium", nil
		}
		return "", exec.ErrNotFound
	}, func(_ context.Context, executable string) error {
		if executable != "/snap/bin/chromium" {
			t.Fatalf("unexpected executable probe: %q", executable)
		}
		return probeErr
	})
	if err == nil {
		t.Fatal("expected error for broken auto-discovered browser")
	}
	if !strings.Contains(err.Error(), "available on PATH") || !strings.Contains(err.Error(), chromeExecutableEnvVar) || !strings.Contains(err.Error(), "/snap/bin/chromium") {
		t.Fatalf("expected actionable auto-discovery error, got %q", err)
	}
	if !errors.Is(err, probeErr) {
		t.Fatalf("expected wrapped probe error, got %v", err)
	}
}

func TestChromiumBrowserBrowseFailsPreflightWithoutLaunchingBrowser(t *testing.T) {
	public := netip.MustParseAddr("93.184.216.34")
	browser := &ChromiumBrowser{
		limits:   DefaultLimits(),
		resolver: staticResolver{records: map[string][]netip.Addr{"example.com": {public}}},
		getenv: func(string) string {
			return " /missing/chrome "
		},
		lookPath: func(candidate string) (string, error) {
			if candidate != "/missing/chrome" {
				t.Fatalf("unexpected executable lookup: %q", candidate)
			}
			return "", exec.ErrNotFound
		},
		probeExecutable: func(context.Context, string) error {
			t.Fatal("probe should not run when configured executable is missing")
			return nil
		},
	}

	_, err := browser.Browse(context.Background(), BrowseRequest{URL: "https://example.com"})
	if err == nil {
		t.Fatal("expected browse preflight error")
	}
	if !strings.Contains(err.Error(), chromeExecutableEnvVar) || !strings.Contains(err.Error(), "/missing/chrome") {
		t.Fatalf("expected browse error to mention configured executable guidance, got %q", err)
	}
}
