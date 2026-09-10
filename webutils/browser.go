package webutils

import (
	"context"
	"errors"
	"fmt"
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

	allocatorOptions := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(profileDir),
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoDefaultBrowserCheck,
		chromedp.NoFirstRun,
	)
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
