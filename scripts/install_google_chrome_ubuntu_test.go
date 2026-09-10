package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func installScriptPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(filename), "install-google-chrome-ubuntu.sh")
}

func TestInstallGoogleChromeUbuntuScriptSyntax(t *testing.T) {
	scriptPath := installScriptPath(t)
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	cmd := exec.Command(bashPath, "-n", scriptPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n %s failed: %v\n%s", scriptPath, err, output)
	}
}

func TestInstallGoogleChromeUbuntuScriptStaticChecks(t *testing.T) {
	scriptPath := installScriptPath(t)
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	text := string(content)

	required := []string{
		"/usr/bin/google-chrome",
		"dpkg --print-architecture",
		"google-chrome-stable",
		"signed-by=",
		"linux_signing_key.pub",
		"EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796",
	}
	for _, needle := range required {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected script to contain %q", needle)
		}
	}

	forbidden := []string{
		"apt-key",
		"snap install",
	}
	for _, needle := range forbidden {
		if strings.Contains(text, needle) {
			t.Fatalf("expected script not to contain %q", needle)
		}
	}
}
