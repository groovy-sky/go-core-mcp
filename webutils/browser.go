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
	defaultMaxScreenshotB   = 1 << 20
	defaultMaxActions       = 8
	defaultMaxActionType    = 32
	defaultMaxSelectorChars = 512
	defaultMaxActionValue   = 2000
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
)

const (
	browserActionWaitVisible = "wait_visible"
	browserActionClick       = "click"
	browserActionSetValue    = "set_value"
	browserActionType        = "type"
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
	MaxRedirects        int
	MaxScreenshotBytes  int
	MaxActions          int
	MaxActionTypeChars  int
	MaxSelectorChars    int
	MaxActionValueChars int
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
		MaxActions:          defaultMaxActions,
		MaxActionTypeChars:  defaultMaxActionType,
		MaxSelectorChars:    defaultMaxSelectorChars,
		MaxActionValueChars: defaultMaxActionValue,
	}
}

type BrowserAction struct {
	Type     string `json:"type"`
	Selector string `json:"selector"`
	Value    string `json:"value,omitempty"`
}

type BrowseRequest struct {
	URL               string
	MaxTextChars      int
	CaptureScreenshot bool
	ScreenshotMode    string
	Actions           []BrowserAction
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
	if err := validateBrowserActions(req.Actions, limits); err != nil {
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
	}
	for i, action := range req.Actions {
		steps, err := chromedpActionsForBrowserAction(i, action)
		if err != nil {
			return BrowseResult{}, err
		}
		actions = append(actions, steps...)
	}
	actions = append(actions,
		chromedp.Location(&finalURL),
		chromedp.Title(&title),
		chromedp.Evaluate(`(() => (document.body ? document.body.innerText : ""))()`, &text),
		chromedp.Evaluate(`(() => (document.baseURI || window.location.href || ""))()`, &pageBaseURL),
		chromedp.Evaluate(`(() => (document.documentElement ? document.documentElement.outerHTML : ""))()`, &renderedHTML),
		chromedp.Evaluate(`(() => Array.from(document.querySelectorAll("a[href]")).map((a) => ({text: (a.innerText || a.textContent || "").trim(), url: a.href})))()`, &rawLinks),
	)
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
	if limits.MaxActions <= 0 {
		limits.MaxActions = defaults.MaxActions
	}
	if limits.MaxActionTypeChars <= 0 {
		limits.MaxActionTypeChars = defaults.MaxActionTypeChars
	}
	if limits.MaxSelectorChars <= 0 {
		limits.MaxSelectorChars = defaults.MaxSelectorChars
	}
	if limits.MaxActionValueChars <= 0 {
		limits.MaxActionValueChars = defaults.MaxActionValueChars
	}
	return limits
}

func validateBrowserActions(actions []BrowserAction, limits Limits) error {
	limits = normalizeLimits(limits)
	if len(actions) > limits.MaxActions {
		return fmt.Errorf("browser actions exceed the maximum count of %d", limits.MaxActions)
	}
	for i, action := range actions {
		actionType := strings.TrimSpace(action.Type)
		if actionType == "" {
			return fmt.Errorf("browser action %d type is required", i+1)
		}
		if len(action.Type) > limits.MaxActionTypeChars {
			return fmt.Errorf("browser action %d type is too long", i+1)
		}
		selector := strings.TrimSpace(action.Selector)
		if selector == "" {
			return fmt.Errorf("browser action %d (%s) selector is required", i+1, actionType)
		}
		if len(action.Selector) > limits.MaxSelectorChars {
			return fmt.Errorf("browser action %d (%s) selector is too long", i+1, actionType)
		}
		trimmedValue := strings.TrimSpace(action.Value)
		switch actionType {
		case browserActionWaitVisible, browserActionClick:
			if trimmedValue != "" {
				return fmt.Errorf("browser action %d (%s) does not accept a value", i+1, actionType)
			}
		case browserActionSetValue, browserActionType:
			if trimmedValue == "" {
				return fmt.Errorf("browser action %d (%s) value is required", i+1, actionType)
			}
		default:
			return fmt.Errorf("unsupported browser action type %q", actionType)
		}
		if len(action.Value) > limits.MaxActionValueChars {
			return fmt.Errorf("browser action %d (%s) value is too long", i+1, actionType)
		}
	}
	return nil
}

func chromedpActionsForBrowserAction(index int, action BrowserAction) ([]chromedp.Action, error) {
	actionType := strings.TrimSpace(action.Type)
	selector := strings.TrimSpace(action.Selector)
	switch actionType {
	case browserActionWaitVisible:
		return []chromedp.Action{
			wrapBrowserAction(index, actionType, chromedp.WaitVisible(selector, chromedp.ByQuery)),
		}, nil
	case browserActionClick:
		return []chromedp.Action{
			wrapBrowserAction(index, actionType,
				chromedp.WaitVisible(selector, chromedp.ByQuery),
				chromedp.Click(selector, chromedp.ByQuery),
				waitForStablePage(),
			),
		}, nil
	case browserActionSetValue:
		return []chromedp.Action{
			wrapBrowserAction(index, actionType,
				chromedp.WaitVisible(selector, chromedp.ByQuery),
				chromedp.SetValue(selector, action.Value, chromedp.ByQuery),
			),
		}, nil
	case browserActionType:
		return []chromedp.Action{
			wrapBrowserAction(index, actionType,
				chromedp.WaitVisible(selector, chromedp.ByQuery),
				chromedp.Focus(selector, chromedp.ByQuery),
				chromedp.SendKeys(selector, action.Value, chromedp.ByQuery),
			),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported browser action type %q", actionType)
	}
}

func wrapBrowserAction(index int, actionType string, actions ...chromedp.Action) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for _, action := range actions {
			if err := action.Do(ctx); err != nil {
				return fmt.Errorf("browser action %d (%s) failed: %w", index+1, actionType, err)
			}
		}
		return nil
	})
}

func waitForStablePage() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var (
			lastURL        string
			lastReadyState string
		)
		for {
			if err := chromedp.Sleep(100 * time.Millisecond).Do(ctx); err != nil {
				return err
			}
			var currentURL string
			if err := chromedp.Evaluate(`window.location.href || ""`, &currentURL).Do(ctx); err != nil {
				return err
			}
			var readyState string
			if err := chromedp.Evaluate(`document.readyState || ""`, &readyState).Do(ctx); err != nil {
				return err
			}
			if readyState == "complete" && currentURL != "" && currentURL == lastURL && lastReadyState == "complete" {
				return nil
			}
			lastURL = currentURL
			lastReadyState = readyState
		}
	})
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
