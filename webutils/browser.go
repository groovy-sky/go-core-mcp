package webutils

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	readability "codeberg.org/readeck/go-readability/v2"
	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
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
	defaultMaxScreenshotB   = 1 << 20
	chromeExecutableEnvVar  = "WEBUTILS_CHROME_EXECUTABLE"
	chromeArgsEnvVar        = "WEBUTILS_CHROME_ARGS"
	defaultChromeExecutable = "/usr/bin/chromium"
	screenshotModeViewport  = "viewport"
	screenshotModeFullPage  = "full_page"
	contentFormatMarkdown   = "markdown"
	contentFormatText       = "text"
	extractionReadability   = "readability_markdown"
	extractionInnerText     = "inner_text"
)

type Limits struct {
	Timeout             time.Duration
	DefaultMaxTextChars int
	MaxAllowedTextChars int
	MaxLinks            int
	MaxLinkTextChars    int
	MaxRedirects        int
	MaxScreenshotBytes  int
}

func DefaultLimits() Limits {
	return Limits{
		Timeout:             defaultTimeout,
		DefaultMaxTextChars: defaultMaxTextChars,
		MaxAllowedTextChars: maxAllowedTextChars,
		MaxLinks:            defaultMaxLinks,
		MaxLinkTextChars:    defaultMaxLinkTextChars,
		MaxRedirects:        8,
		MaxScreenshotBytes:  defaultMaxScreenshotB,
	}
}

type BrowseRequest struct {
	URL               string
	MaxTextChars      int
	CaptureScreenshot bool
	ScreenshotMode    string
}

type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type BrowseResult struct {
	FinalURL         string `json:"final_url"`
	Title            string `json:"title"`
	Content          string `json:"content"`
	ContentFormat    string `json:"content_format"`
	ExtractionMethod string `json:"extraction_method"`
	VisibleText      string `json:"visible_text"`
	Links            []Link `json:"links"`
	Truncated        bool   `json:"truncated"`
	ScreenshotPNG    []byte `json:"-"`
}

type Browser interface {
	Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error)
}

type ChromiumBrowser struct {
	limits   Limits
	resolver policyResolver
}

func NewChromiumBrowser(limits Limits) *ChromiumBrowser {
	return &ChromiumBrowser{limits: normalizeLimits(limits), resolver: defaultResolver()}
}

var errScreenshotTooLarge = errors.New("screenshot exceeds maximum byte limit")

func (b *ChromiumBrowser) Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error) {
	limits := normalizeLimits(b.limits)
	maxChars := req.MaxTextChars
	if maxChars <= 0 {
		maxChars = limits.DefaultMaxTextChars
	}
	if maxChars > limits.MaxAllowedTextChars {
		maxChars = limits.MaxAllowedTextChars
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

	timeout := limits.Timeout
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
				tooMany := redirects > limits.MaxRedirects
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
		finalURL      string
		title         string
		text          string
		renderedHTML  string
		pageBaseURL   string
		rawLinks      []map[string]string
		screenshotPNG []byte
	)
	actions := []chromedp.Action{
		chromedp.Navigate(targetURL.String()),
		chromedp.Location(&finalURL),
		chromedp.Title(&title),
		chromedp.Evaluate(`(() => (document.body ? document.body.innerText : ""))()`, &text),
		chromedp.Evaluate(`(() => (document.baseURI || window.location.href || ""))()`, &pageBaseURL),
		chromedp.Evaluate(`(() => (document.documentElement ? document.documentElement.outerHTML : ""))()`, &renderedHTML),
		chromedp.Evaluate(`(() => Array.from(document.querySelectorAll("a[href]")).map((a) => ({text: (a.innerText || a.textContent || "").trim(), url: a.href})))()`, &rawLinks),
	}
	if req.CaptureScreenshot {
		if normalizeScreenshotMode(req.ScreenshotMode) == screenshotModeFullPage {
			actions = append(actions, chromedp.FullScreenshot(&screenshotPNG, 90))
		} else {
			actions = append(actions, chromedp.CaptureScreenshot(&screenshotPNG))
		}
	}
	if err := chromedp.Run(browserCtx, actions...); err != nil {
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
	if req.CaptureScreenshot && len(screenshotPNG) > limits.MaxScreenshotBytes {
		return BrowseResult{}, fmt.Errorf("%w: got %d bytes, limit is %d bytes", errScreenshotTooLarge, len(screenshotPNG), limits.MaxScreenshotBytes)
	}

	visibleText := strings.TrimSpace(text)
	content, contentFormat, extractionMethod := extractContentFromHTML(renderedHTML, pageBaseURL, visibleText)
	content, contentTruncated := clampString(content, maxChars)
	visibleText, visibleTextTruncated := clampString(visibleText, maxChars)
	truncated := contentTruncated || visibleTextTruncated

	links := make([]Link, 0, limits.MaxLinks)
	for _, item := range rawLinks {
		if len(links) >= limits.MaxLinks {
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
		linkText, linkTextTruncated := clampString(strings.TrimSpace(item["text"]), limits.MaxLinkTextChars)
		truncated = truncated || linkTextTruncated
		links = append(links, Link{Text: linkText, URL: linkURL})
	}

	return BrowseResult{
		FinalURL:         finalURL,
		Title:            strings.TrimSpace(title),
		Content:          content,
		ContentFormat:    contentFormat,
		ExtractionMethod: extractionMethod,
		VisibleText:      visibleText,
		Links:            links,
		Truncated:        truncated,
		ScreenshotPNG:    screenshotPNG,
	}, nil
}

func normalizeLimits(limits Limits) Limits {
	defaults := DefaultLimits()
	if limits.Timeout <= 0 {
		limits.Timeout = defaults.Timeout
	}
	if limits.DefaultMaxTextChars <= 0 {
		limits.DefaultMaxTextChars = defaults.DefaultMaxTextChars
	}
	if limits.MaxAllowedTextChars <= 0 {
		limits.MaxAllowedTextChars = defaults.MaxAllowedTextChars
	}
	if limits.DefaultMaxTextChars > limits.MaxAllowedTextChars {
		limits.DefaultMaxTextChars = limits.MaxAllowedTextChars
	}
	if limits.MaxLinks <= 0 {
		limits.MaxLinks = defaults.MaxLinks
	}
	if limits.MaxLinkTextChars <= 0 {
		limits.MaxLinkTextChars = defaults.MaxLinkTextChars
	}
	if limits.MaxRedirects <= 0 {
		limits.MaxRedirects = defaults.MaxRedirects
	}
	if limits.MaxScreenshotBytes <= 0 {
		limits.MaxScreenshotBytes = defaults.MaxScreenshotBytes
	}
	return limits
}

func normalizeScreenshotMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), screenshotModeFullPage) {
		return screenshotModeFullPage
	}
	return screenshotModeViewport
}

func extractContentFromHTML(renderedHTML, pageURL, fallbackText string) (content, contentFormat, extractionMethod string) {
	fallbackText = strings.TrimSpace(fallbackText)
	pageURL = strings.TrimSpace(pageURL)
	if markdown, err := extractReadableMarkdown(renderedHTML, pageURL); err == nil {
		return markdown, contentFormatMarkdown, extractionReadability
	}
	return fallbackText, contentFormatText, extractionInnerText
}

func extractReadableMarkdown(renderedHTML, pageURL string) (string, error) {
	if strings.TrimSpace(renderedHTML) == "" {
		return "", errors.New("rendered HTML is empty")
	}
	parsedURL, err := url.Parse(pageURL)
	if err != nil || !parsedURL.IsAbs() {
		return "", errors.New("page URL is invalid for extraction")
	}
	article, err := readability.FromReader(strings.NewReader(renderedHTML), parsedURL)
	if err != nil {
		return "", err
	}
	if article.Node == nil {
		return "", errors.New("no readability content")
	}
	htmlContent := strings.Builder{}
	if err := article.RenderHTML(&htmlContent); err != nil {
		return "", err
	}
	rendered := strings.TrimSpace(htmlContent.String())
	if rendered == "" {
		return "", errors.New("readability content is empty")
	}
	markdown, err := htmltomarkdown.ConvertString(rendered, converter.WithDomain(parsedURL.String()))
	if err != nil {
		return "", err
	}
	markdown = strings.TrimSpace(markdown)
	if !hasUsefulContent(markdown) {
		return "", errors.New("markdown content is empty")
	}
	return markdown, nil
}

func hasUsefulContent(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	return strings.IndexFunc(content, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r)
	}) >= 0
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
