package webutils

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	chromeExecutableEnvVar  = "WEBUTILS_CHROME_EXECUTABLE"
	chromeArgsEnvVar        = "WEBUTILS_CHROME_ARGS"
	defaultChromeExecutable = "/usr/bin/chromium"
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
	limits   Limits
	resolver policyResolver
}

func NewChromiumBrowser(limits Limits) *ChromiumBrowser {
	return &ChromiumBrowser{limits: limits, resolver: defaultResolver()}
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
	execPath, err := resolveChromeExecutablePath()
	if err != nil {
		return BrowseResult{}, err
	}
	execOptions, err := resolveChromeArgs()
	if err != nil {
		return BrowseResult{}, err
	}

	profileDir, err := os.MkdirTemp("", "webutils-chromium-*")
	if err != nil {
		return BrowseResult{}, fmt.Errorf("create browser profile: %w", err)
	}
	defer os.RemoveAll(profileDir)

	timeout := b.limits.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	allocatorOptions := append([]chromedp.ExecAllocatorOption{chromedp.ExecPath(execPath)}, chromedp.DefaultExecAllocatorOptions[:]...)
	allocatorOptions = append(allocatorOptions,
		chromedp.UserDataDir(profileDir),
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoDefaultBrowserCheck,
		chromedp.NoFirstRun,
	)
	allocatorOptions = append(allocatorOptions, execOptions...)
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
				// Fetch callbacks run on a separate goroutine, so execute CDP
				// actions through chromedp.Run to bind the active target executor.
				requestURL := strings.TrimSpace(evt.Request.URL)
				if _, err := validateAndResolveURL(browserCtx, b.resolver, requestURL); err != nil {
					if failErr := chromedp.Run(browserCtx, fetch.FailRequest(evt.RequestID, network.ErrorReasonBlockedByClient)); failErr != nil {
						setCheckErr(fmt.Errorf("fail request: %w", failErr))
						return
					}
					setCheckErr(fmt.Errorf("blocked request %q: %w", requestURL, err))
					return
				}
				if err := chromedp.Run(browserCtx, fetch.ContinueRequest(evt.RequestID)); err != nil {
					setCheckErr(fmt.Errorf("continue request: %w", err))
				}
			}(typed)
		}
	})

	if err := chromedp.Run(browserCtx,
		network.Enable(),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}),
	); err != nil {
		return BrowseResult{}, fmt.Errorf("enable browser interception: %w", err)
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
		return BrowseResult{}, fmt.Errorf("navigation failed: %w", err)
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

func clampString(value string, limit int) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	return value[:limit], true
}

func resolveChromeExecutablePath() (string, error) {
	return resolveChromeExecutable(os.LookupEnv, os.Stat, defaultChromeExecutable)
}

func resolveChromeArgs() ([]chromedp.ExecAllocatorOption, error) {
	return resolveChromeArgsFromEnv(os.LookupEnv)
}

func resolveChromeArgsFromEnv(lookupEnv func(string) (string, bool)) ([]chromedp.ExecAllocatorOption, error) {
	configured, ok := lookupEnv(chromeArgsEnvVar)
	if !ok {
		return nil, nil
	}
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return nil, nil
	}

	flags, err := parseChromeArgs(configured)
	if err != nil {
		return nil, err
	}
	options := make([]chromedp.ExecAllocatorOption, 0, len(flags))
	for _, flag := range flags {
		if flag.hasValue {
			options = append(options, chromedp.Flag(flag.name, flag.value))
			continue
		}
		options = append(options, chromedp.Flag(flag.name, true))
	}
	return options, nil
}

type chromeFlag struct {
	name     string
	value    string
	hasValue bool
}

func parseChromeArgs(configured string) ([]chromeFlag, error) {
	args := strings.Fields(configured)
	flags := make([]chromeFlag, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			return nil, fmt.Errorf("%s contains unsupported Chromium argument %q: expected --flag or --flag=value syntax", chromeArgsEnvVar, arg)
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%s contains an empty Chromium flag in %q", chromeArgsEnvVar, arg)
		}
		flags = append(flags, chromeFlag{name: name, value: value, hasValue: hasValue})
	}
	return flags, nil
}

func resolveChromeExecutable(lookupEnv func(string) (string, bool), stat func(string) (fs.FileInfo, error), defaultPath string) (string, error) {
	if configured, ok := lookupEnv(chromeExecutableEnvVar); ok {
		if configured = strings.TrimSpace(configured); configured != "" {
			if err := validateExecutablePath(configured, stat); err != nil {
				return "", fmt.Errorf("%s=%q is not usable: %w. Install Chrome/Chromium at that path or unset %s to use %q instead. Snap-wrapper chromium-browser launchers are unsupported in this container", chromeExecutableEnvVar, configured, err, chromeExecutableEnvVar, defaultPath)
			}
			return configured, nil
		}
	}
	if err := validateExecutablePath(defaultPath, stat); err != nil {
		return "", fmt.Errorf("default Chromium executable %q is not usable: %w. Install Debian's chromium package there or set %s to a valid Chrome/Chromium executable. Snap-wrapper chromium-browser launchers are unsupported in this container", defaultPath, err, chromeExecutableEnvVar)
	}
	return defaultPath, nil
}

func validateExecutablePath(path string, stat func(string) (fs.FileInfo, error)) error {
	info, err := stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("path does not exist")
		}
		return err
	}
	if info.IsDir() {
		return errors.New("path is a directory")
	}
	if info.Mode()&0o111 == 0 {
		return errors.New("path is not executable")
	}
	return nil
}
