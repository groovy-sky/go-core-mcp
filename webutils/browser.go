package webutils

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	defaultTimeout          = 20 * time.Second
	defaultMaxTextChars     = 4000
	maxAllowedTextChars     = 12000
	defaultMaxLinks         = 40
	defaultMaxLinkTextChars = 200
	chromiumProbeTimeout    = 5 * time.Second
	chromeExecutableEnvVar  = "WEBUTILS_CHROME_EXECUTABLE"
)

type Limits struct {
	Timeout             time.Duration
	DefaultMaxTextChars int
	MaxAllowedTextChars int
	MaxLinks            int
	MaxLinkTextChars    int
	MaxRedirects        int
}

func DefaultLimits() Limits {
	return Limits{
		Timeout:             defaultTimeout,
		DefaultMaxTextChars: defaultMaxTextChars,
		MaxAllowedTextChars: maxAllowedTextChars,
		MaxLinks:            defaultMaxLinks,
		MaxLinkTextChars:    defaultMaxLinkTextChars,
		MaxRedirects:        8,
	}
}

type BrowseRequest struct {
	URL          string
	MaxTextChars int
}

type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type BrowseResult struct {
	FinalURL    string `json:"final_url"`
	Title       string `json:"title"`
	VisibleText string `json:"visible_text"`
	Links       []Link `json:"links"`
	Truncated   bool   `json:"truncated"`
}

type Browser interface {
	Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error)
}

type ChromiumBrowser struct {
	limits          Limits
	resolver        policyResolver
	getenv          func(string) string
	lookPath        func(string) (string, error)
	probeExecutable func(context.Context, string) error
}

func NewChromiumBrowser(limits Limits) *ChromiumBrowser {
	return &ChromiumBrowser{
		limits:          limits,
		resolver:        defaultResolver(),
		getenv:          os.Getenv,
		lookPath:        exec.LookPath,
		probeExecutable: probeChromiumExecutable,
	}
}

func (b *ChromiumBrowser) Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error) {
	maxChars := req.MaxTextChars
	if maxChars <= 0 {
		maxChars = b.limits.DefaultMaxTextChars
	}
	if maxChars > b.limits.MaxAllowedTextChars {
		maxChars = b.limits.MaxAllowedTextChars
	}
	targetURL, err := validateAndResolveURL(ctx, b.resolver, req.URL)
	if err != nil {
		return BrowseResult{}, err
	}

	timeout := b.limits.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	configuredExecutable := configuredChromiumExecutable(b.getenv)
	if err := validateChromiumExecutable(runCtx, configuredExecutable, b.lookPath, b.getenv, b.probeExecutable); err != nil {
		return BrowseResult{}, err
	}

	profileDir, err := os.MkdirTemp("", "webutils-chromium-*")
	if err != nil {
		return BrowseResult{}, fmt.Errorf("create browser profile: %w", err)
	}
	defer os.RemoveAll(profileDir)

	var browserOutput bytes.Buffer
	allocatorOptions := execAllocatorOptions(profileDir, configuredExecutable, &browserOutput)
	allocCtx, allocCancel := chromedp.NewExecAllocator(runCtx, allocatorOptions...)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	var (
		redirects   int
		redirectMtx sync.Mutex
		checkErr    error
		checkErrMtx sync.Mutex
	)
	setCheckErr := func(err error) {
		if err == nil {
			return
		}
		checkErrMtx.Lock()
		defer checkErrMtx.Unlock()
		if checkErr == nil {
			checkErr = err
			browserCancel()
		}
	}
	getCheckErr := func() error {
		checkErrMtx.Lock()
		defer checkErrMtx.Unlock()
		return checkErr
	}

	chromedp.ListenTarget(browserCtx, func(event any) {
		switch typed := event.(type) {
		case *network.EventRequestWillBeSent:
			if typed.RedirectResponse != nil {
				redirectMtx.Lock()
				redirects++
				tooMany := redirects > b.limits.MaxRedirects
				redirectMtx.Unlock()
				if tooMany {
					setCheckErr(errors.New("redirect limit exceeded"))
				}
			}
		case *fetch.EventRequestPaused:
			go func(evt *fetch.EventRequestPaused) {
				requestURL := strings.TrimSpace(evt.Request.URL)
				if _, err := validateAndResolveURL(browserCtx, b.resolver, requestURL); err != nil {
					_ = fetch.FailRequest(evt.RequestID, network.ErrorReasonBlockedByClient).Do(browserCtx)
					setCheckErr(fmt.Errorf("blocked request %q: %w", requestURL, err))
					return
				}
				if err := fetch.ContinueRequest(evt.RequestID).Do(browserCtx); err != nil {
					setCheckErr(fmt.Errorf("continue request: %w", err))
				}
			}(typed)
		}
	})

	if err := chromedp.Run(browserCtx,
		network.Enable(),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}),
	); err != nil {
		return BrowseResult{}, wrapBrowserLaunchError("enable browser interception", err, browserOutput.String())
	}

	var (
		finalURL string
		title    string
		text     string
		rawLinks []map[string]string
	)
	if err := chromedp.Run(browserCtx,
		chromedp.Navigate(targetURL.String()),
		chromedp.Location(&finalURL),
		chromedp.Title(&title),
		chromedp.Evaluate(`(() => (document.body ? document.body.innerText : ""))()`, &text),
		chromedp.Evaluate(`(() => Array.from(document.querySelectorAll("a[href]")).map((a) => ({text: (a.innerText || a.textContent || "").trim(), url: a.href})))()`, &rawLinks),
	); err != nil {
		if blocked := getCheckErr(); blocked != nil {
			return BrowseResult{}, blocked
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(browserCtx.Err(), context.DeadlineExceeded) {
			return BrowseResult{}, context.DeadlineExceeded
		}
		return BrowseResult{}, wrapBrowserLaunchError("navigation failed", err, browserOutput.String())
	}
	if blocked := getCheckErr(); blocked != nil {
		return BrowseResult{}, blocked
	}
	if _, err := validateAndResolveURL(ctx, b.resolver, finalURL); err != nil {
		return BrowseResult{}, fmt.Errorf("final destination is not allowed: %w", err)
	}

	text, truncated := clampString(strings.TrimSpace(text), maxChars)
	links := make([]Link, 0, b.limits.MaxLinks)
	for _, item := range rawLinks {
		if len(links) >= b.limits.MaxLinks {
			truncated = true
			break
		}
		linkURL := strings.TrimSpace(item["url"])
		if linkURL == "" {
			continue
		}
		if _, err := validateAndResolveURL(ctx, b.resolver, linkURL); err != nil {
			continue
		}
		linkText, linkTextTruncated := clampString(strings.TrimSpace(item["text"]), b.limits.MaxLinkTextChars)
		truncated = truncated || linkTextTruncated
		links = append(links, Link{Text: linkText, URL: linkURL})
	}

	return BrowseResult{
		FinalURL:    finalURL,
		Title:       strings.TrimSpace(title),
		VisibleText: text,
		Links:       links,
		Truncated:   truncated,
	}, nil
}

func configuredChromiumExecutable(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	return strings.TrimSpace(getenv(chromeExecutableEnvVar))
}

func execAllocatorOptions(profileDir, executable string, output io.Writer) []chromedp.ExecAllocatorOption {
	options := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(profileDir),
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoDefaultBrowserCheck,
		chromedp.NoFirstRun,
	)
	if executable != "" {
		options = append(options, chromedp.ExecPath(executable))
	}
	if output != nil {
		options = append(options, chromedp.CombinedOutput(output))
	}
	return options
}

func validateChromiumExecutable(ctx context.Context, configured string, lookPath func(string) (string, error), getenv func(string) string, probe func(context.Context, string) error) error {
	if lookPath == nil {
		return errors.New("browser executable lookup is not configured")
	}
	if probe == nil {
		return errors.New("browser preflight probe is not configured")
	}
	if configured != "" {
		resolved, err := lookPath(configured)
		if err != nil {
			return fmt.Errorf("%s is set to %q, but that executable could not be found. Set %s to a working Chromium/Chrome executable path such as /usr/bin/chromium: %w", chromeExecutableEnvVar, configured, chromeExecutableEnvVar, err)
		}
		if err := probe(ctx, resolved); err != nil {
			return fmt.Errorf("%s is set to %q, but Chromium/Chrome could not be started from %q. Fix that executable or point %s to a working browser path: %w", chromeExecutableEnvVar, configured, resolved, chromeExecutableEnvVar, err)
		}
		return nil
	}
	resolved, err := discoverChromiumExecutable(lookPath, getenv)
	if err != nil {
		return fmt.Errorf("Chromium or Chrome is required for browse_url. Install it and make sure it is available on PATH, or configure %s to a working executable path: %w", chromeExecutableEnvVar, err)
	}
	if err := probe(ctx, resolved); err != nil {
		return fmt.Errorf("Chromium or Chrome was discovered at %q, but it could not be started. Ensure a working browser is installed and available on PATH, or configure %s to a working executable path: %w", resolved, chromeExecutableEnvVar, err)
	}
	return nil
}

func discoverChromiumExecutable(lookPath func(string) (string, error), getenv func(string) string) (string, error) {
	if lookPath == nil {
		return "", errors.New("executable lookup is not configured")
	}
	candidates := chromiumExecutableCandidates(runtime.GOOS, getenv)
	for _, candidate := range candidates {
		resolved, err := lookPath(candidate)
		if err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("no Chromium/Chrome executable found in the checked candidates (%s)", strings.Join(candidates, ", "))
}

func chromiumExecutableCandidates(goos string, getenv func(string) string) []string {
	if getenv == nil {
		getenv = os.Getenv
	}
	switch goos {
	case "darwin":
		return []string{
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"Chromium",
			"Google Chrome",
			"chromium",
			"google-chrome",
			"chrome",
		}
	case "windows":
		userProfile := getenv("USERPROFILE")
		return []string{
			"chrome",
			"chrome.exe",
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			joinWindowsPath(userProfile, `AppData\Local\Google\Chrome\Application\chrome.exe`),
			joinWindowsPath(userProfile, `AppData\Local\Chromium\Application\chrome.exe`),
		}
	default:
		return []string{
			"headless_shell",
			"headless-shell",
			"chromium",
			"chromium-browser",
			"google-chrome",
			"google-chrome-stable",
			"google-chrome-beta",
			"google-chrome-unstable",
			"/usr/bin/google-chrome",
			"/usr/local/bin/chrome",
			"/snap/bin/chromium",
			"chrome",
		}
	}
}

func joinWindowsPath(base, tail string) string {
	base = strings.TrimRight(base, `\/`)
	if base == "" {
		return tail
	}
	return base + `\` + tail
}

func probeChromiumExecutable(ctx context.Context, executable string) error {
	probeCtx, cancel := context.WithTimeout(ctx, chromiumProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, executable, "--version")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return wrapBrowserLaunchError("browser preflight failed", err, output.String())
	}
	return nil
}

func wrapBrowserLaunchError(message string, err error, browserOutput string) error {
	browserOutput = strings.TrimSpace(browserOutput)
	if browserOutput == "" {
		return fmt.Errorf("%s: %w", message, err)
	}
	return fmt.Errorf("%s: %w (browser output: %s)", message, err, browserOutput)
}

func clampString(value string, limit int) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	return value[:limit], true
}
