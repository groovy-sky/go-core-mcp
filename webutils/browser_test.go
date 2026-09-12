package webutils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNormalizeLimitsUsesDefaultsForZeroValues(t *testing.T) {
	t.Parallel()

	normalized := normalizeLimits(Limits{})
	defaults := DefaultLimits()
	if normalized != defaults {
		t.Fatalf("expected normalized limits to match defaults, got %+v want %+v", normalized, defaults)
	}
}

func TestExtractContentFromHTMLStrategies(t *testing.T) {
	t.Parallel()

	longBody := strings.Repeat("Detailed rendered fallback content. ", 12)
	testCases := []struct {
		name         string
		html         string
		pageURL      string
		fallbackText string
		wantContent  string
		wantFormat   string
		wantMethod   string
		contains     []string
		notContains  []string
	}{
		{
			name: "prefers readability for article",
			html: `<!doctype html>
<html>
<head><title>Ignored</title></head>
<body>
  <nav>navigation noise</nav>
  <article>
    <h1>Hello World</h1>
    <p>Intro with <a href="/docs">docs</a>.</p>
    <ul><li>One</li><li>Two</li></ul>
    <pre><code>fmt.Println("ok")</code></pre>
    <table><tr><th>Name</th><th>Value</th></tr><tr><td>A</td><td>B</td></tr></table>
  </article>
</body>
</html>`,
			pageURL:      "https://example.com/base/page",
			fallbackText: "fallback text",
			wantFormat:   contentFormatMarkdown,
			wantMethod:   extractionReadability,
			contains:     []string{"Hello World", "[docs](https://example.com/docs)", "- One", "fmt.Println(\"ok\")", "Name"},
		},
		{
			name: "uses semantic rendered dom fallback for app page",
			html: `<!doctype html>
<html>
<body>
  <nav>Primary navigation</nav>
  <div id="cookie-banner">Accept cookies</div>
  <style>.noise { color: red; }</style>
  <script>window.unwanted = "script noise"</script>
  <dialog open>Overlay dialog</dialog>
  <div role="main">
    <h1>Workspace Dashboard</h1>
    <p>Open the <a href="/reports">reports</a> for the latest status.</p>
    <ul><li>Queued</li><li>Ready</li></ul>
    <table>
      <tr><th>Name</th><th>Value</th></tr>
      <tr><td>Build</td><td>Passing</td></tr>
    </table>
    <pre><code>go test ./...</code></pre>
    <img alt="Diagram" src="/images/diagram.png">
  </div>
</body>
</html>`,
			pageURL:      "https://example.com/base/page",
			fallbackText: "fallback text",
			wantFormat:   contentFormatMarkdown,
			wantMethod:   extractionRenderedDOM,
			contains: []string{
				"Workspace Dashboard",
				"[reports](https://example.com/reports)",
				"Queued",
				"Name",
				"go test ./...",
				"https://example.com/images/diagram.png",
			},
			notContains: []string{"Primary navigation", "Accept cookies", "script noise", "Overlay dialog"},
		},
		{
			name:         "falls back to visible text when html unusable",
			html:         `<!doctype html><html><body><script>ignored()</script><style>body{display:none}</style><template>hidden</template></body></html>`,
			pageURL:      "https://example.com",
			fallbackText: " visible fallback ",
			wantContent:  "visible fallback",
			wantFormat:   contentFormatText,
			wantMethod:   extractionInnerText,
		},
		{
			name: "prefers richer rendered dom over thin readability",
			html: `<!doctype html>
<html>
<body>
  <article>
    <h1>Status</h1>
    <p>Short summary only.</p>
  </article>
  <main>
    <h1>Workspace Guide</h1>
    <p>` + longBody + `</p>
    <ul><li>Open backlog</li><li>Run checks</li><li>Ship release</li></ul>
    <table>
      <tr><th>Area</th><th>State</th></tr>
      <tr><td>Build</td><td>Green</td></tr>
    </table>
    <pre><code>go test ./webutils</code></pre>
  </main>
</body>
</html>`,
			pageURL:      "https://example.com/docs/app",
			fallbackText: "fallback text",
			wantFormat:   contentFormatMarkdown,
			wantMethod:   extractionRenderedDOM,
			contains:     []string{"Workspace Guide", "go test ./webutils", "Open backlog", "Area"},
			notContains:  []string{"fallback text"},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			content, format, method := extractContentFromHTML(tc.html, tc.pageURL, tc.fallbackText)
			if tc.wantContent != "" && content != tc.wantContent {
				t.Fatalf("expected content %q, got %q", tc.wantContent, content)
			}
			if format != tc.wantFormat {
				t.Fatalf("expected format %q, got %q", tc.wantFormat, format)
			}
			if method != tc.wantMethod {
				t.Fatalf("expected extraction method %q, got %q with content %q", tc.wantMethod, method, content)
			}
			for _, needle := range tc.contains {
				if !strings.Contains(content, needle) {
					t.Fatalf("expected content to contain %q, got %q", needle, content)
				}
			}
			for _, needle := range tc.notContains {
				if strings.Contains(content, needle) {
					t.Fatalf("expected content to omit %q, got %q", needle, content)
				}
			}
		})
	}
}

func TestClampStringAppliesTruncationLimit(t *testing.T) {
	t.Parallel()

	content, _, _ := extractContentFromHTML(`<html><body><article><h1>Heading</h1><p>alpha beta gamma</p></article></body></html>`, "https://example.com", "fallback")
	clamped, truncated := clampString(content, 12)
	if !truncated {
		t.Fatal("expected content to be truncated")
	}
	if len([]rune(clamped)) != 12 {
		t.Fatalf("expected clamped content rune length 12, got %d", len([]rune(clamped)))
	}
}

func TestClampStringPreservesUTF8Runes(t *testing.T) {
	t.Parallel()

	clamped, truncated := clampString("🙂世界abc", 3)
	if !truncated {
		t.Fatal("expected UTF-8 string to be truncated")
	}
	if clamped != "🙂世界" {
		t.Fatalf("expected UTF-8-safe truncation, got %q", clamped)
	}
	if !utf8.ValidString(clamped) {
		t.Fatalf("expected clamped string to remain valid UTF-8, got %q", clamped)
	}
}

func TestNormalizeSearchLinksFiltersAndBounds(t *testing.T) {
	t.Parallel()

	links, truncated := normalizeSearchLinks(context.Background(), staticResolver{
		records: map[string][]netip.Addr{
			"go.dev":          {netip.MustParseAddr("216.239.32.21")},
			"pkg.go.dev":      {netip.MustParseAddr("216.239.36.21")},
			"private.example": {netip.MustParseAddr("127.0.0.1")},
		},
	}, []map[string]string{
		{"title": "Go", "url": "https://go.dev", "snippet": "The Go programming language", "source": "result"},
		{"title": "Duplicate Go", "url": "https://go.dev", "snippet": "duplicate", "source": "result"},
		{"title": "Private", "url": "https://private.example", "snippet": "blocked", "source": "result"},
		{"title": "Pkg", "url": "https://pkg.go.dev", "snippet": strings.Repeat("x", 300), "source": "result"},
	}, 2, 50)
	if len(links) != 2 {
		t.Fatalf("expected 2 links, got %d", len(links))
	}
	if links[0].URL != "https://go.dev" || links[0].Rank != 1 {
		t.Fatalf("unexpected first link: %+v", links[0])
	}
	if links[1].URL != "https://pkg.go.dev" || links[1].Rank != 2 {
		t.Fatalf("unexpected second link: %+v", links[1])
	}
	if !truncated {
		t.Fatal("expected truncation due max results or snippet clamp")
	}
}

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
		name      string
		lookupEnv func(string) (string, bool)
	}{
		{
			name:      "unset",
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

func TestChromiumBrowserBrowseContinuesInterceptedRequests(t *testing.T) {
	chromePath := findChromeExecutable(t)
	server, tracker := newBrowserFixtureServer(t, false)
	wrapperPath := makeChromeWrapper(t, chromePath, fixtureListenerIP(t, server), "allowed.example")
	t.Setenv(chromeExecutableEnvVar, wrapperPath)
	t.Setenv(chromeArgsEnvVar, "--no-sandbox --disable-dev-shm-usage")

	browser := NewChromiumBrowser(DefaultLimits())
	browser.limits.Timeout = 15 * time.Second
	browser.resolver = staticResolver{
		records: map[string][]netip.Addr{
			"allowed.example": {netip.MustParseAddr("93.184.216.34")},
		},
	}

	result, err := browser.Browse(context.Background(), BrowseRequest{
		URL: fixtureURL(t, server, "allowed.example", "/"),
	})
	if err != nil {
		t.Fatalf("Browse returned error: %v", err)
	}
	if result.Title != "Fixture OK" {
		t.Fatalf("expected title %q, got %q", "Fixture OK", result.Title)
	}
	for _, needle := range []string{"fixture main page", "script loaded"} {
		if !strings.Contains(result.VisibleText, needle) {
			t.Fatalf("expected visible text to contain %q, got %q", needle, result.VisibleText)
		}
	}
	if result.FinalURL != fixtureURL(t, server, "allowed.example", "/") {
		t.Fatalf("expected final URL %q, got %q", fixtureURL(t, server, "allowed.example", "/"), result.FinalURL)
	}
	if tracker.count("allowed.example", "/style.css") == 0 {
		t.Fatal("expected stylesheet request to be continued")
	}
	if tracker.count("allowed.example", "/app.js") == 0 {
		t.Fatal("expected script request to be continued")
	}
	if tracker.count("allowed.example", "/image.svg") == 0 {
		t.Fatal("expected image request to be continued")
	}
}

func TestChromiumBrowserBrowseBlocksDisallowedSubresourceRequest(t *testing.T) {
	chromePath := findChromeExecutable(t)
	server, tracker := newBrowserFixtureServer(t, true)
	wrapperPath := makeChromeWrapper(t, chromePath, fixtureListenerIP(t, server), "allowed.example", "blocked.example")
	t.Setenv(chromeExecutableEnvVar, wrapperPath)
	t.Setenv(chromeArgsEnvVar, "--no-sandbox --disable-dev-shm-usage")

	browser := NewChromiumBrowser(DefaultLimits())
	browser.limits.Timeout = 15 * time.Second
	browser.resolver = staticResolver{
		records: map[string][]netip.Addr{
			"allowed.example": {netip.MustParseAddr("93.184.216.34")},
			"blocked.example": {netip.MustParseAddr("127.0.0.1")},
		},
	}

	_, err := browser.Browse(context.Background(), BrowseRequest{
		URL: fixtureURL(t, server, "allowed.example", "/"),
	})
	if err == nil {
		t.Fatal("expected blocked subresource request to fail")
	}
	if !strings.Contains(err.Error(), `blocked request "https://blocked.example`) {
		t.Fatalf("expected blocked request error, got %v", err)
	}
	if !strings.Contains(err.Error(), errHostNotPublic.Error()) {
		t.Fatalf("expected blocked request to preserve URL-validation failure, got %v", err)
	}
	if strings.Contains(err.Error(), "invalid context") {
		t.Fatalf("expected blocked request path to avoid invalid context, got %v", err)
	}
	if tracker.count("blocked.example", "/blocked.js") != 0 {
		t.Fatal("expected blocked subresource request to be failed before reaching the server")
	}
}

func TestChromiumBrowserBrowseCapturesScreenshot(t *testing.T) {
	chromePath := findChromeExecutable(t)
	server, _ := newBrowserFixtureServer(t, false)
	wrapperPath := makeChromeWrapper(t, chromePath, fixtureListenerIP(t, server), "allowed.example")
	t.Setenv(chromeExecutableEnvVar, wrapperPath)
	t.Setenv(chromeArgsEnvVar, "--no-sandbox --disable-dev-shm-usage")

	browser := NewChromiumBrowser(DefaultLimits())
	browser.limits.Timeout = 15 * time.Second
	browser.resolver = staticResolver{
		records: map[string][]netip.Addr{
			"allowed.example": {netip.MustParseAddr("93.184.216.34")},
		},
	}

	result, err := browser.Browse(context.Background(), BrowseRequest{
		URL:               fixtureURL(t, server, "allowed.example", "/"),
		CaptureScreenshot: true,
		ScreenshotMode:    screenshotModeViewport,
	})
	if err != nil {
		t.Fatalf("Browse returned error: %v", err)
	}
	if len(result.ScreenshotPNG) == 0 {
		t.Fatal("expected screenshot bytes in browse result")
	}
}

func TestChromiumBrowserBrowseRejectsOversizedScreenshot(t *testing.T) {
	chromePath := findChromeExecutable(t)
	server, _ := newBrowserFixtureServer(t, false)
	wrapperPath := makeChromeWrapper(t, chromePath, fixtureListenerIP(t, server), "allowed.example")
	t.Setenv(chromeExecutableEnvVar, wrapperPath)
	t.Setenv(chromeArgsEnvVar, "--no-sandbox --disable-dev-shm-usage")

	limits := DefaultLimits()
	limits.MaxScreenshotBytes = 1
	browser := NewChromiumBrowser(limits)
	browser.limits.Timeout = 15 * time.Second
	browser.resolver = staticResolver{
		records: map[string][]netip.Addr{
			"allowed.example": {netip.MustParseAddr("93.184.216.34")},
		},
	}

	_, err := browser.Browse(context.Background(), BrowseRequest{
		URL:               fixtureURL(t, server, "allowed.example", "/"),
		CaptureScreenshot: true,
	})
	if err == nil {
		t.Fatal("expected oversized screenshot to fail")
	}
	if !errors.Is(err, errScreenshotTooLarge) {
		t.Fatalf("expected oversized screenshot error, got %v", err)
	}
}

func findChromeExecutable(t *testing.T) string {
	t.Helper()

	for _, candidate := range []string{
		"/usr/bin/chromium",
		"/usr/bin/google-chrome",
		"/opt/google/chrome/chrome",
		"/usr/bin/chromium-browser",
	} {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	t.Skip("no Chrome/Chromium executable available for browser integration test")
	return ""
}

func makeChromeWrapper(t *testing.T, chromePath, ip string, hosts ...string) string {
	t.Helper()

	rules := make([]string, 0, len(hosts))
	for _, host := range hosts {
		rules = append(rules, fmt.Sprintf("MAP %s %s", host, ip))
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %q %q --ignore-certificate-errors --no-proxy-server \"$@\"\n",
		chromePath,
		"--host-resolver-rules="+strings.Join(rules, ","),
	)
	path := filepath.Join(t.TempDir(), "chromium-wrapper")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write Chromium wrapper: %v", err)
	}
	return path
}

func fixtureURL(t *testing.T, server *httptest.Server, host, path string) string {
	t.Helper()

	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split fixture listener address: %v", err)
	}
	return "https://" + host + ":" + port + path
}

func fixtureListenerIP(t *testing.T, server *httptest.Server) string {
	t.Helper()

	host, _, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split fixture listener address: %v", err)
	}
	return host
}

type browserFixtureTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func (t *browserFixtureTracker) record(host, path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[host+" "+path]++
}

func (t *browserFixtureTracker) count(host, path string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[host+" "+path]
}

func newBrowserFixtureServer(t *testing.T, includeBlockedScript bool) (*httptest.Server, *browserFixtureTracker) {
	t.Helper()

	tracker := &browserFixtureTracker{counts: make(map[string]int)}
	listenIP := findNonLoopbackIPv4(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort(listenIP, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", listenIP, err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if parsedHost, _, err := net.SplitHostPort(r.Host); err == nil {
			host = parsedHost
		}
		tracker.record(host, r.URL.Path)

		blockedScript := ""
		if includeBlockedScript {
			blockedScript = fmt.Sprintf(`<script defer src="https://blocked.example:%s/blocked.js"></script>`, port)
		}

		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<!doctype html><html><head><title>Fixture OK</title><link rel="stylesheet" href="/style.css"><script defer src="/app.js"></script>%s</head><body><main>fixture main page</main><span id="script-loaded">pending</span><img alt="fixture" src="/image.svg"><a href="/next">next page</a></body></html>`, blockedScript)
		case "/style.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			_, _ = w.Write([]byte("body { color: rgb(1, 2, 3); }"))
		case "/app.js":
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = w.Write([]byte(`document.getElementById("script-loaded").textContent = "script loaded";`))
		case "/image.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10" fill="green"/></svg>`))
		case "/next":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><html><head><title>Fixture Next</title></head><body>next page</body></html>`))
		case "/blocked.js":
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = w.Write([]byte(`window.blockedScriptLoaded = true;`))
		default:
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, tracker
}

func findNonLoopbackIPv4(t *testing.T) string {
	t.Helper()

	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list network interfaces: %v", err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !ip.IsGlobalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	t.Skip("no non-loopback IPv4 address available for browser integration test")
	return ""
}
