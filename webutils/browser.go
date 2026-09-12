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
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"golang.org/x/net/html"
)

const (
	defaultTimeout          = 20 * time.Second
	defaultMaxTextChars     = 4000
	maxAllowedTextChars     = 12000
	defaultMaxLinks         = 40
	defaultMaxLinkTextChars = 200
	defaultMaxSearchResults = 5
	maxAllowedSearchResults = 10
	maxSearchQueryRunes     = 300
	defaultMaxScreenshotB   = 1 << 20
	chromeExecutableEnvVar  = "WEBUTILS_CHROME_EXECUTABLE"
	chromeArgsEnvVar        = "WEBUTILS_CHROME_ARGS"
	defaultChromeExecutable = "/usr/bin/chromium"
	screenshotModeViewport  = "viewport"
	screenshotModeFullPage  = "full_page"
	contentFormatMarkdown   = "markdown"
	contentFormatText       = "text"
	extractionReadability   = "readability_markdown"
	extractionRenderedDOM   = "rendered_dom_markdown"
	extractionInnerText     = "inner_text"
	searchEngineDuckDuckGo  = "duckduckgo"
)

const (
	readabilityThinTextRunes = 220
	inadequateTextRunes      = 12
	renderedDOMMainBonus     = 60
	renderedDOMRoleMainBonus = 50
	renderedDOMArticleBonus  = 40
	renderedDOMBodyBonus     = 0
)

type markdownQuality struct {
	textRunes       int
	score           int
	semanticSignals int
}

type renderedDOMCandidate struct {
	node        *html.Node
	sourceBonus int
	sourceKind  string
}

type renderedDOMResult struct {
	markdown   string
	quality    markdownQuality
	sourceKind string
}

type Limits struct {
	Timeout             time.Duration
	DefaultMaxTextChars int
	MaxAllowedTextChars int
	MaxLinks            int
	MaxLinkTextChars    int
	DefaultMaxResults   int
	MaxSearchResults    int
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
		DefaultMaxResults:   defaultMaxSearchResults,
		MaxSearchResults:    maxAllowedSearchResults,
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
	Text   string `json:"text"`
	URL    string `json:"url"`
	Rank   int    `json:"rank,omitempty"`
	Source string `json:"source,omitempty"`
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

type SearchRequest struct {
	Query      string
	MaxResults int
	Engine     string
}

type SearchResult struct {
	Engine      string `json:"engine"`
	Query       string `json:"query"`
	FinalURL    string `json:"final_url"`
	Title       string `json:"title"`
	Results     []Link `json:"results"`
	VisibleText string `json:"visible_text"`
	Truncated   bool   `json:"truncated"`
}

type Browser interface {
	Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error)
	Search(ctx context.Context, req SearchRequest) (SearchResult, error)
}

type ChromiumBrowser struct {
	limits   Limits
	resolver policyResolver
}

func NewChromiumBrowser(limits Limits) *ChromiumBrowser {
	return &ChromiumBrowser{limits: normalizeLimits(limits), resolver: defaultResolver()}
}

var errScreenshotTooLarge = errors.New("screenshot exceeds maximum byte limit")
var errSearchQueryRequired = errors.New("search query is required")
var errSearchEngineUnsupported = errors.New("search engine is not supported")

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
	baseURL := strings.TrimSpace(pageBaseURL)
	if baseURL == "" {
		baseURL = finalURL
	}
	content, contentFormat, extractionMethod := extractContentFromHTML(renderedHTML, baseURL, visibleText)
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
		links = append(links, Link{Text: linkText, URL: linkURL, Rank: len(links) + 1, Source: "page_anchor"})
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

func (b *ChromiumBrowser) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	limits := normalizeLimits(b.limits)
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return SearchResult{}, errSearchQueryRequired
	}
	if len([]rune(query)) > maxSearchQueryRunes {
		runes := []rune(query)
		query = string(runes[:maxSearchQueryRunes])
	}
	engine := normalizeSearchEngine(req.Engine)
	if engine == "" {
		return SearchResult{}, errSearchEngineUnsupported
	}
	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = limits.DefaultMaxResults
	}
	if maxResults > limits.MaxSearchResults {
		maxResults = limits.MaxSearchResults
	}
	searchURL, err := searchURLForQuery(engine, query)
	if err != nil {
		return SearchResult{}, err
	}

	targetURL, err := validateAndResolveURL(ctx, b.resolver, searchURL)
	if err != nil {
		return SearchResult{}, err
	}
	execPath, err := resolveChromeExecutablePath()
	if err != nil {
		return SearchResult{}, err
	}
	execOptions, err := resolveChromeArgs()
	if err != nil {
		return SearchResult{}, err
	}

	profileDir, err := os.MkdirTemp("", "webutils-chromium-*")
	if err != nil {
		return SearchResult{}, fmt.Errorf("create browser profile: %w", err)
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
		return SearchResult{}, fmt.Errorf("enable browser interception: %w", err)
	}

	var (
		finalURL    string
		title       string
		visibleText string
		rawResults  []map[string]string
	)
	actions := []chromedp.Action{
		chromedp.Navigate(targetURL.String()),
		chromedp.Location(&finalURL),
		chromedp.Title(&title),
		chromedp.Evaluate(`(() => (document.body ? document.body.innerText : ""))()`, &visibleText),
		chromedp.Evaluate(`(() => {
			const out = [];
			const seen = new Set();
			const push = (title, url, snippet, source) => {
				const normalizedURL = (url || "").trim();
				if (!normalizedURL || seen.has(normalizedURL)) return;
				seen.add(normalizedURL);
				out.push({
					title: (title || "").trim(),
					url: normalizedURL,
					snippet: (snippet || "").trim(),
					source: (source || "unknown").trim(),
				});
			};
			const cardSelectors = [
				'article',
				'.result',
				'.result__body',
				'[data-testid="result"]',
				'.web-result',
			];
			for (const selector of cardSelectors) {
				for (const card of document.querySelectorAll(selector)) {
					const anchor = card.querySelector('a[href]');
					if (!anchor) continue;
					const heading = card.querySelector('h1, h2, h3, h4');
					const snippetEl = card.querySelector('p, .snippet, .result__snippet, [data-testid="result-snippet"]');
					push(heading ? heading.innerText : anchor.innerText, anchor.href, snippetEl ? snippetEl.innerText : "", selector);
				}
			}
			for (const anchor of document.querySelectorAll('h3 a[href], h2 a[href], .result__a[href], a[data-testid="result-title-a"]')) {
				const parent = anchor.closest('article, .result, .result__body, [data-testid="result"], .web-result');
				const snippetEl = parent ? parent.querySelector('p, .snippet, .result__snippet, [data-testid="result-snippet"]') : null;
				push(anchor.innerText, anchor.href, snippetEl ? snippetEl.innerText : "", "heading_anchor");
			}
			return out;
		})()`, &rawResults),
	}
	if err := chromedp.Run(browserCtx, actions...); err != nil {
		if blocked := getCheckErr(); blocked != nil {
			return SearchResult{}, blocked
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(browserCtx.Err(), context.DeadlineExceeded) {
			return SearchResult{}, context.DeadlineExceeded
		}
		return SearchResult{}, fmt.Errorf("navigation failed: %w", err)
	}
	if blocked := getCheckErr(); blocked != nil {
		return SearchResult{}, blocked
	}
	if _, err := validateAndResolveURL(ctx, b.resolver, finalURL); err != nil {
		return SearchResult{}, fmt.Errorf("final destination is not allowed: %w", err)
	}
	results, resultsTruncated := normalizeSearchLinks(ctx, b.resolver, rawResults, maxResults, limits.MaxLinkTextChars)
	visibleText, textTruncated := clampString(strings.TrimSpace(visibleText), limits.MaxAllowedTextChars)
	return SearchResult{
		Engine:      engine,
		Query:       query,
		FinalURL:    finalURL,
		Title:       strings.TrimSpace(title),
		Results:     results,
		VisibleText: visibleText,
		Truncated:   resultsTruncated || textTruncated,
	}, nil
}

func normalizeSearchEngine(engine string) string {
	normalized := strings.ToLower(strings.TrimSpace(engine))
	if normalized == "" {
		return searchEngineDuckDuckGo
	}
	switch normalized {
	case searchEngineDuckDuckGo:
		return searchEngineDuckDuckGo
	default:
		return ""
	}
}

func searchURLForQuery(engine, query string) (string, error) {
	switch engine {
	case searchEngineDuckDuckGo:
		encoded := url.QueryEscape(strings.TrimSpace(query))
		return "https://duckduckgo.com/html/?q=" + encoded, nil
	default:
		return "", errSearchEngineUnsupported
	}
}

func normalizeSearchLinks(ctx context.Context, resolver policyResolver, raw []map[string]string, maxResults int, maxTextChars int) ([]Link, bool) {
	links := make([]Link, 0, maxResults)
	seen := map[string]struct{}{}
	truncated := false
	for _, item := range raw {
		if len(links) >= maxResults {
			truncated = true
			break
		}
		linkURL := strings.TrimSpace(item["url"])
		if linkURL == "" {
			continue
		}
		if _, exists := seen[linkURL]; exists {
			continue
		}
		if _, err := validateAndResolveURL(ctx, resolver, linkURL); err != nil {
			continue
		}
		seen[linkURL] = struct{}{}
		title, titleTruncated := clampString(strings.TrimSpace(item["title"]), maxTextChars)
		snippet, snippetTruncated := clampString(strings.TrimSpace(item["snippet"]), maxTextChars)
		source, _ := clampString(strings.TrimSpace(item["source"]), 40)
		text := strings.TrimSpace(title)
		if snippet != "" {
			if text != "" {
				text += " — " + snippet
			} else {
				text = snippet
			}
		}
		if text == "" {
			text = linkURL
		}
		truncated = truncated || titleTruncated || snippetTruncated
		links = append(links, Link{
			Text:   text,
			URL:    linkURL,
			Rank:   len(links) + 1,
			Source: source,
		})
	}
	return links, truncated
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
	if limits.DefaultMaxResults <= 0 {
		limits.DefaultMaxResults = defaults.DefaultMaxResults
	}
	if limits.MaxSearchResults <= 0 {
		limits.MaxSearchResults = defaults.MaxSearchResults
	}
	if limits.DefaultMaxResults > limits.MaxSearchResults {
		limits.DefaultMaxResults = limits.MaxSearchResults
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
	readabilityMarkdown, readabilityErr := extractReadableMarkdown(renderedHTML, pageURL)
	renderedDOM, renderedDOMErr := extractRenderedDOMMarkdownResult(renderedHTML, pageURL)
	readabilityQuality := assessMarkdownQuality(readabilityMarkdown, 0)
	if readabilityErr == nil && isInadequateMarkdown(readabilityQuality) {
		readabilityErr = errors.New("readability content is inadequate")
	}
	switch {
	case readabilityErr == nil && renderedDOMErr == nil:
		if shouldPreferRenderedDOM(readabilityQuality, renderedDOM.quality, renderedDOM.sourceKind) {
			return renderedDOM.markdown, contentFormatMarkdown, extractionRenderedDOM
		}
		return readabilityMarkdown, contentFormatMarkdown, extractionReadability
	case readabilityErr == nil:
		return readabilityMarkdown, contentFormatMarkdown, extractionReadability
	case renderedDOMErr == nil:
		return renderedDOM.markdown, contentFormatMarkdown, extractionRenderedDOM
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

func extractRenderedDOMMarkdown(renderedHTML, pageURL string) (string, error) {
	result, err := extractRenderedDOMMarkdownResult(renderedHTML, pageURL)
	if err != nil {
		return "", err
	}
	return result.markdown, nil
}

func extractRenderedDOMMarkdownResult(renderedHTML, pageURL string) (renderedDOMResult, error) {
	if strings.TrimSpace(renderedHTML) == "" {
		return renderedDOMResult{}, errors.New("rendered HTML is empty")
	}
	parsedURL, err := url.Parse(pageURL)
	if err != nil || !parsedURL.IsAbs() {
		return renderedDOMResult{}, errors.New("page URL is invalid for extraction")
	}
	document, err := html.Parse(strings.NewReader(renderedHTML))
	if err != nil {
		return renderedDOMResult{}, err
	}
	candidates := collectRenderedDOMCandidates(document)
	if len(candidates) == 0 {
		return renderedDOMResult{}, errors.New("no rendered DOM candidate")
	}
	var (
		best  renderedDOMResult
		found bool
	)
	for _, candidate := range candidates {
		cloned := cloneHTMLNode(candidate.node)
		cleanRenderedDOMNode(cloned)
		rendered := strings.TrimSpace(renderHTMLNode(cloned))
		if rendered == "" {
			continue
		}
		markdown, err := convertRenderedHTMLToMarkdown(rendered, parsedURL)
		if err != nil {
			continue
		}
		markdown = strings.TrimSpace(markdown)
		quality := assessMarkdownQuality(markdown, candidate.sourceBonus)
		if quality.textRunes == 0 {
			continue
		}
		if !found || quality.score > best.quality.score || (quality.score == best.quality.score && quality.textRunes > best.quality.textRunes) {
			best = renderedDOMResult{
				markdown:   markdown,
				quality:    quality,
				sourceKind: candidate.sourceKind,
			}
			found = true
		}
	}
	if !found {
		return renderedDOMResult{}, errors.New("rendered DOM markdown content is empty")
	}
	return best, nil
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
	if limit <= 0 {
		return value, false
	}
	runes := 0
	for idx := range value {
		if runes == limit {
			return value[:idx], true
		}
		runes++
	}
	return value, false
}

func collectRenderedDOMCandidates(document *html.Node) []renderedDOMCandidate {
	var (
		candidates []renderedDOMCandidate
		seen       = map[*html.Node]struct{}{}
	)
	appendMatches := func(nodes []*html.Node, sourceBonus int) {
		for _, node := range nodes {
			if _, ok := seen[node]; ok {
				continue
			}
			seen[node] = struct{}{}
			sourceKind := "body"
			switch sourceBonus {
			case renderedDOMMainBonus:
				sourceKind = "main"
			case renderedDOMRoleMainBonus:
				sourceKind = "role_main"
			case renderedDOMArticleBonus:
				sourceKind = "article"
			}
			candidates = append(candidates, renderedDOMCandidate{node: node, sourceBonus: sourceBonus, sourceKind: sourceKind})
		}
	}
	appendMatches(findElements(document, func(node *html.Node) bool {
		return hasTag(node, "main")
	}), renderedDOMMainBonus)
	appendMatches(findElements(document, func(node *html.Node) bool {
		return hasAttrValue(node, "role", "main")
	}), renderedDOMRoleMainBonus)
	appendMatches(findElements(document, func(node *html.Node) bool {
		return hasTag(node, "article")
	}), renderedDOMArticleBonus)
	appendMatches(findElements(document, func(node *html.Node) bool {
		return hasTag(node, "body")
	}), renderedDOMBodyBonus)
	return candidates
}

func findElements(root *html.Node, match func(*html.Node) bool) []*html.Node {
	if root == nil {
		return nil
	}
	var matches []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if match(node) {
			matches = append(matches, node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return matches
}

func cleanRenderedDOMNode(node *html.Node) {
	if node == nil {
		return
	}
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		if shouldRemoveRenderedDOMNode(child) {
			node.RemoveChild(child)
		} else {
			cleanRenderedDOMNode(child)
		}
		child = next
	}
}

func shouldRemoveRenderedDOMNode(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	switch strings.ToLower(node.Data) {
	case "script", "style", "template", "noscript", "dialog", "nav", "aside", "footer", "iframe":
		return true
	}
	if hasBooleanAttr(node, "hidden") || hasBooleanAttr(node, "inert") {
		return true
	}
	for _, role := range []string{"dialog", "alertdialog", "navigation", "complementary", "contentinfo", "banner"} {
		if hasAttrValue(node, "role", role) {
			return true
		}
	}
	if hasAttrValue(node, "aria-hidden", "true") || hasAttrValue(node, "aria-modal", "true") {
		return true
	}
	return hasNoiseIdentifier(node)
}

func hasNoiseIdentifier(node *html.Node) bool {
	for _, key := range []string{"id", "class", "aria-label", "data-testid", "data-test", "data-qa"} {
		value := strings.ToLower(strings.TrimSpace(getAttr(node, key)))
		if value == "" {
			continue
		}
		for _, token := range []string{"cookie", "consent", "gdpr", "onetrust", "modal", "overlay", "popup", "drawer"} {
			if strings.Contains(value, token) {
				return true
			}
		}
	}
	return false
}

func hasTag(node *html.Node, tag string) bool {
	return node != nil && node.Type == html.ElementNode && strings.EqualFold(node.Data, tag)
}

func hasAttrValue(node *html.Node, key, value string) bool {
	return strings.EqualFold(strings.TrimSpace(getAttr(node, key)), value)
}

func hasBooleanAttr(node *html.Node, key string) bool {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			return true
		}
	}
	return false
}

func getAttr(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func cloneHTMLNode(node *html.Node) *html.Node {
	if node == nil {
		return nil
	}
	cloned := &html.Node{
		Type:      node.Type,
		DataAtom:  node.DataAtom,
		Data:      node.Data,
		Namespace: node.Namespace,
		Attr:      append([]html.Attribute(nil), node.Attr...),
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		cloned.AppendChild(cloneHTMLNode(child))
	}
	return cloned
}

func renderHTMLNode(node *html.Node) string {
	if node == nil {
		return ""
	}
	var builder strings.Builder
	if err := html.Render(&builder, node); err != nil {
		return ""
	}
	return builder.String()
}

func assessMarkdownQuality(content string, sourceBonus int) markdownQuality {
	content = strings.TrimSpace(content)
	if !hasUsefulContent(content) {
		return markdownQuality{}
	}
	var (
		textRunes      int
		headings       int
		lists          int
		tables         int
		codeBlocks     int
		blockQuotes    int
		images         int
		links          int
		inCodeBlock    bool
		tableSeparator bool
	)
	for _, r := range content {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			textRunes++
		}
	}
	images = strings.Count(content, "![")
	links = strings.Count(content, "](")
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			if !inCodeBlock {
				codeBlocks++
			}
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "#"):
			headings++
		case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "), isOrderedMarkdownListLine(trimmed):
			lists++
		case strings.HasPrefix(trimmed, "> "):
			blockQuotes++
		}
		if strings.Contains(trimmed, "---") && strings.Count(trimmed, "|") >= 2 {
			tableSeparator = true
		}
	}
	if tableSeparator {
		tables = 1
	}
	semanticSignals := clampCount(headings, 2) + clampCount(lists, 4) + clampCount(tables, 1) + clampCount(codeBlocks, 2) + clampCount(blockQuotes, 2) + clampCount(images, 2)
	score := textRunes + sourceBonus + 80*clampCount(headings, 2) + 40*clampCount(lists, 4) + 120*clampCount(tables, 1) + 120*clampCount(codeBlocks, 2) + 50*clampCount(blockQuotes, 2) + 20*clampCount(images, 2) + 10*clampCount(links, 5)
	return markdownQuality{
		textRunes:       textRunes,
		score:           score,
		semanticSignals: semanticSignals,
	}
}

func shouldPreferRenderedDOM(readabilityQuality, renderedDOMQuality markdownQuality, renderedDOMSource string) bool {
	if readabilityQuality.textRunes == 0 || renderedDOMQuality.textRunes == 0 {
		return false
	}
	if renderedDOMSource == "article" {
		return false
	}
	if readabilityQuality.textRunes >= readabilityThinTextRunes {
		return renderedDOMQuality.textRunes >= readabilityQuality.textRunes*4/5 &&
			renderedDOMQuality.semanticSignals >= readabilityQuality.semanticSignals+1 &&
			renderedDOMQuality.score >= readabilityQuality.score+50
	}
	return renderedDOMQuality.textRunes >= readabilityQuality.textRunes*4/5 &&
		renderedDOMQuality.semanticSignals >= readabilityQuality.semanticSignals+1 &&
		renderedDOMQuality.score >= readabilityQuality.score+50
}

func isInadequateMarkdown(quality markdownQuality) bool {
	return quality.textRunes > 0 && quality.textRunes < inadequateTextRunes && quality.semanticSignals == 0
}

func convertRenderedHTMLToMarkdown(rendered string, pageURL *url.URL) (string, error) {
	conv := converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
		),
	)
	conv.Register.Plugin(table.NewTablePlugin())
	return conv.ConvertString(rendered, converter.WithDomain(pageURL.String()))
}

func isOrderedMarkdownListLine(line string) bool {
	line = strings.TrimSpace(line)
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	return digits > 0 && digits+1 < len(line) && line[digits] == '.' && line[digits+1] == ' '
}

func clampCount(value, max int) int {
	if value > max {
		return max
	}
	return value
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
