package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/chromedp"
	readability "github.com/go-shiori/go-readability"
)

// ─────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────

const (
	duckDuckGoLiteURL = "https://lite.duckduckgo.com/lite/"
	defaultTopN       = 5
)

// searchLLMBaseURL returns the chat-completions URL for the configured vLLM instance.
func searchLLMBaseURL() string {
	base := os.Getenv("MAIN_GPU_URL")
	base = strings.TrimRight(base, "/")
	return base + "/chat/completions"
}

// searchLLMAPIKey returns the API key for the configured vLLM instance.
func searchLLMAPIKey() string {
	return os.Getenv("OPENAI_API_KEY")
}

// searchLLMModel returns the default model name for the configured vLLM instance.
// Reads SEARCH_MODEL env first, falls back to MAIN_MODEL, then a generic default.
func searchLLMModel() string {
	if m := os.Getenv("SEARCH_MODEL"); m != "" {
		return m
	}
	if m := os.Getenv("MAIN_MODEL"); m != "" {
		return m
	}
	return "default"
}

// ─────────────────────────────────────────────
// Data Models
// ─────────────────────────────────────────────

// SearchResult holds a single parsed result from the search engine.
type SearchResult struct {
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	URL     string `json:"url"`
}

// CrawledPage holds the cleaned main-text content of a fetched URL.
type CrawledPage struct {
	URL     string
	Title   string
	Content string
}

// LLMMessage is a single turn in an OpenAI-compatible chat messages array.
type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLMRequest is the payload sent to the completion endpoint.
type LLMRequest struct {
	Model       string       `json:"model"`
	Messages    []LLMMessage `json:"messages"`
	Temperature float64      `json:"temperature"`
	Stream      bool         `json:"stream"`
}

// LLMStreamChunk is a single server-sent-events data chunk (streaming).
type LLMStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// ─────────────────────────────────────────────
// Anti-Bot: Stealth Header System
// ─────────────────────────────────────────────

// BrowserHeaders holds the dynamic browser fingerprint extracted from the
// host's real Chromium installation. Updated every 24 hours via cron.
type BrowserHeaders struct {
	UserAgent string
	SecChUa   string
	Platform  string
	Mobile    string
}

// currentHeaders is the live atomic pointer to the active BrowserHeaders.
// Using atomic.Pointer guarantees goroutine-safe reads with zero lock contention.
var currentHeaders atomic.Pointer[BrowserHeaders]

// hasBrowser caches the result of the browser environment check performed at
// startup. Avoids repeated exec.LookPath calls on every crawl.
var hasBrowser bool

// ─────────────────────────────────────────────
// Initialization & Environment Check
// ─────────────────────────────────────────────

func init() {
	// Check if chome is installled
	allocCtx, allocCancel := chromedp.NewExecAllocator(
		context.Background(),
		chromedp.DefaultExecAllocatorOptions[:]...,
	)
	defer allocCancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := chromedp.Run(ctx,
		chromedp.Navigate("about:blank"),
	)
	if err != nil {
		// os/exec.Error {Name: "google-chrome", Err: error(*errors.errorString) *{s: "executable file not found in $PATH"}}
		// Check if the error is "command not found"
		if errors.Is(err, exec.ErrNotFound) {
			hasBrowser = false
		}
	}

	// Seed with a known-good static Chrome profile so every outgoing request
	// looks plausible immediately on startup, before the dynamic extraction
	// runs. This prevents the WAF anomaly heuristic from tripping during the
	// first few requests.
	staticHeaders := &BrowserHeaders{
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		SecChUa:   `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		Platform:  `"Windows"`,
		Mobile:    "?0",
	}
	currentHeaders.Store(staticHeaders)
}

// checkBrowserInstalled scans PATH for any known Chromium/Chrome binary.
// The result is stored in hasBrowser and never rechecked at runtime.

// ─────────────────────────────────────────────
// Dynamic Header Updates & 24-Hour Cron
// ─────────────────────────────────────────────

// StartHeaderManager should be called exactly once from main().
// It runs an immediate header extraction on boot so the very first real
// requests use native browser headers, then refreshes every 24 hours.
// If no browser is installed it returns immediately; the static seed headers
// remain active for the process lifetime.
func StartHeaderManager() {
	if !hasBrowser {
		log.Println("[cron] skipping dynamic header updates (no browser installed)")
		return
	}

	// Extract immediately so we don't rely on static headers any longer than
	// necessary – important if the static profile becomes stale.
	updateHeaders()

	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			updateHeaders()
		}
	}()
}

// updateHeaders launches a short-lived headless Chrome session, evaluates a
// JS snippet to read the browser's own navigator properties, and atomically
// stores the result in currentHeaders. On any failure the previous value is
// retained so headers never become empty.
func updateHeaders() {
	log.Println("[cron] extracting native browser headers via chromedp…")

	allocCtx, allocCancel := chromedp.NewExecAllocator(
		context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-gpu", true),
		)...,
	)
	defer allocCancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Read the real navigator values from inside the browser process.
	// This guarantees the UA string and Sec-CH-UA hints exactly match what
	// the installed Chromium version would send over the wire.
	js := `(function() {
		let secChUa = "";
		if (navigator.userAgentData) {
			secChUa = navigator.userAgentData.brands
				.map(b => '"' + b.brand + '";v="' + b.version + '"')
				.join(", ");
		}
		return {
			ua:       navigator.userAgent,
			secChUa:  secChUa,
			platform: navigator.userAgentData ? navigator.userAgentData.platform : "Windows",
			mobile:   (navigator.userAgentData && navigator.userAgentData.mobile) ? "?1" : "?0"
		};
	})()`

	var res map[string]string
	err := chromedp.Run(ctx,
		chromedp.Navigate("about:blank"),
		chromedp.Evaluate(js, &res),
	)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			hasBrowser = false
		}
		log.Printf("[cron] extraction failed, retaining previous headers: %v", err)
		return
	}

	newHeaders := &BrowserHeaders{
		UserAgent: res["ua"],
		SecChUa:   res["secChUa"],
		Platform:  fmt.Sprintf(`"%s"`, res["platform"]),
		Mobile:    res["mobile"],
	}
	currentHeaders.Store(newHeaders)
	log.Printf("[cron] headers updated → UA: %s", newHeaders.UserAgent)
}

// ─────────────────────────────────────────────
// Stealth Header Application
// ─────────────────────────────────────────────

// applyStealthHeaders stamps the currently active browser fingerprint onto an
// outgoing request, plus the full set of Fetch-metadata headers that modern
// browsers always include. Missing Sec-Fetch-* headers or Upgrade-Insecure-Requests
// are among the most common WAF anomaly triggers.
func applyStealthHeaders(req *http.Request) {
	h := currentHeaders.Load()

	// Dynamic headers – sourced from the live atomic BrowserHeaders value.
	req.Header.Set("User-Agent", h.UserAgent)
	req.Header.Set("Sec-Ch-Ua", h.SecChUa)
	req.Header.Set("Sec-Ch-Ua-Mobile", h.Mobile)
	req.Header.Set("Sec-Ch-Ua-Platform", h.Platform)

	// Static Fetch-metadata headers – required for a plausible browser profile.
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
}

// ─────────────────────────────────────────────
// Step 1: Query DuckDuckGo Lite
// ─────────────────────────────────────────────

// fetchDDGHTML issues a POST to DuckDuckGo Lite and returns the raw HTML body.
// Stealth headers are applied to avoid WAF anomaly detection.
func fetchDDGHTML(query string) (string, error) {
	form := url.Values{}
	form.Set("q", query)

	req, err := http.NewRequest(http.MethodPost, duckDuckGoLiteURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building DDG request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://lite.duckduckgo.com")
	req.Header.Set("Referer", "https://lite.duckduckgo.com/")
	applyStealthHeaders(req)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("DDG HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading DDG body: %w", err)
	}
	return string(body), nil
}

// ─────────────────────────────────────────────
// Step 2: Deterministic Search Parsing
// ─────────────────────────────────────────────

// extractResults uses goquery to deterministically parse DuckDuckGo Lite HTML.
func extractResults(rawHTML string) []SearchResult {
	doc, _ := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	var results []SearchResult

	doc.Find(".result-link").Each(func(i int, s *goquery.Selection) {
		title := s.Text()
		link, _ := s.Attr("href")
		snippet := s.Closest("tr").Next().Find(".result-snippet").Text()

		results = append(results, SearchResult{
			Title:   strings.TrimSpace(title),
			URL:     link,
			Snippet: strings.TrimSpace(snippet),
		})
	})
	return results
}

func sanitizeText(input string) string {
	// 1. Remove excessive whitespace
	re := regexp.MustCompile(`\s+`)
	output := re.ReplaceAllString(input, " ")
	// 2. Limit maximum length of a single source to 6,000 chars (~1.5k tokens)
	if len(output) > 6000 {
		output = output[:6000] + "..."
	}
	return strings.TrimSpace(output)
}

// ─────────────────────────────────────────────
// Step 3a: JS-Rendered Page Fetching (chromedp)
// ─────────────────────────────────────────────

// fetchRenderedHTML uses a headless Chromium instance to fetch and render a URL.
// Only called when hasBrowser is true.
// Both allocCancel and cancel are always deferred to prevent zombie Chrome processes.
func fetchRenderedHTML(pageURL string) (string, error) {
	allocCtx, allocCancel := chromedp.NewExecAllocator(
		context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-gpu", true),
		)...,
	)
	defer allocCancel() // IMPORTANT: prevents zombie Chrome processes

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel() // IMPORTANT: prevents zombie Chrome processes

	ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var htmlContent string
	err := chromedp.Run(ctx,
		chromedp.Navigate(pageURL),
		chromedp.WaitVisible("body", chromedp.ByQuery),
		chromedp.OuterHTML("html", &htmlContent),
	)
	if err != nil {
		return "", fmt.Errorf("chromedp render %s: %w", pageURL, err)
	}
	return htmlContent, nil
}

// ─────────────────────────────────────────────
// Step 3b: Plain HTTP Fallback Fetcher
// ─────────────────────────────────────────────

// fetchPlainHTTP is the fallback path used when Chromium is not installed.
// It issues a regular GET with stealth headers applied, sufficient for static
// or server-rendered pages. go-readability then strips noise exactly as it
// would on chromedp-rendered output.
//
// This keeps the pipeline functional on ephemeral containers (AWS Lambda,
// Alpine images) where packaging chromium-browser is impractical.
func fetchPlainHTTP(pageURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return "", fmt.Errorf("building plain HTTP request for %s: %w", pageURL, err)
	}

	applyStealthHeaders(req)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("plain HTTP request for %s: %w", pageURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading plain HTTP body for %s: %w", pageURL, err)
	}
	return string(body), nil
}

// ─────────────────────────────────────────────
// Step 3c: Main Text Extraction (go-readability)
// ─────────────────────────────────────────────

// extractMainText runs the Mozilla Readability algorithm over rendered HTML
// to isolate the core article body, stripping ads, navbars, and footers.
func extractMainText(pageURL, renderedHTML string) (CrawledPage, error) {
	parsedURL, err := url.Parse(pageURL)
	if err != nil {
		return CrawledPage{}, fmt.Errorf("parsing URL %s: %w", pageURL, err)
	}

	article, err := readability.FromReader(strings.NewReader(renderedHTML), parsedURL)
	if err != nil {
		return CrawledPage{}, fmt.Errorf("readability extraction for %s: %w", pageURL, err)
	}

	return CrawledPage{
		URL:     pageURL,
		Title:   article.Title,
		Content: article.TextContent,
	}, nil
}

// ─────────────────────────────────────────────
// Cloudflare Pre-Flight Check
// ─────────────────────────────────────────────

// cfBlockedError is returned when a URL is definitively blocked by Cloudflare
// or another WAF. Callers use errors.As to distinguish this from a transient
// network error and drop the result cleanly from citations.
type cfBlockedError struct {
	URL    string
	Status int
	Reason string
}

func (e *cfBlockedError) Error() string {
	return fmt.Sprintf("blocked URL %s (HTTP %d – %s)", e.URL, e.Status, e.Reason)
}

// checkCloudflareBlock issues a HEAD request with stealth headers and inspects
// the response for Cloudflare/WAF signals before spending time on a full
// Chromium session or body read.
//
// Detection layers (cheapest first):
//  1. HTTP 403 Forbidden
//  2. HTTP 429 Too Many Requests
//  3. cf-mitigated header present (active Cloudflare challenge)
//  4. cf-ray header + non-2xx status (Cloudflare edge rejected the request)
func checkCloudflareBlock(pageURL string) error {
	client := &http.Client{
		Timeout: 10 * time.Second,
		// Stop at the first response – a 403 that redirects to a Cloudflare
		// CAPTCHA page must not be silently swallowed as a 200.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest(http.MethodHead, pageURL, nil)
	if err != nil {
		return &cfBlockedError{URL: pageURL, Status: 0, Reason: "invalid URL: " + err.Error()}
	}

	// Apply stealth headers to the pre-flight too – some WAFs block HEAD
	// requests that look like bots before evaluating the path.
	applyStealthHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		// Network-level error is not a definitive block; let the full fetch decide.
		return nil
	}
	defer resp.Body.Close()

	// ── 1 & 2: Hard status-code checks ───────────────────────────────────
	switch resp.StatusCode {
	case http.StatusForbidden: // 403
		return &cfBlockedError{URL: pageURL, Status: resp.StatusCode, Reason: "403 Forbidden"}
	case http.StatusTooManyRequests: // 429
		return &cfBlockedError{URL: pageURL, Status: resp.StatusCode, Reason: "429 Too Many Requests"}
	}

	// ── 3: cf-mitigated – active Cloudflare challenge in progress ─────────
	if v := resp.Header.Get("cf-mitigated"); v != "" {
		return &cfBlockedError{
			URL:    pageURL,
			Status: resp.StatusCode,
			Reason: fmt.Sprintf("Cloudflare mitigation active (cf-mitigated: %s)", v),
		}
	}

	// ── 4: cf-ray + non-2xx – Cloudflare edge rejected the request ───────
	// cf-ray alone just means the request transited Cloudflare's network
	// (true of a large fraction of the internet). It is only a block signal
	// when combined with a 4xx/5xx status code.
	if ray := resp.Header.Get("cf-ray"); ray != "" && resp.StatusCode >= 400 {
		return &cfBlockedError{
			URL:    pageURL,
			Status: resp.StatusCode,
			Reason: fmt.Sprintf("Cloudflare edge rejected request (cf-ray: %s)", ray),
		}
	}

	return nil // URL looks accessible – proceed with full crawl.
}

// ─────────────────────────────────────────────
// Step 3 (combined): Smart Crawl with Failover
// ─────────────────────────────────────────────

// crawlURL is the single entry point for fetching and extracting a page.
// It executes in three sequential stages:
//
//  1. Cloudflare pre-flight – fast HEAD check; returns cfBlockedError on hit.
//  2. Fetch – uses headless Chromium when available (JS-heavy SPAs),
//     otherwise falls back to plain HTTP (static/SSR pages).
//  3. Extract – passes raw HTML through go-readability to isolate article text.
func crawlURL(pageURL string) (CrawledPage, error) {
	// Stage 1: pre-flight – bail before launching Chrome if blocked.
	if err := checkCloudflareBlock(pageURL); err != nil {
		return CrawledPage{}, err
	}

	// Stage 2: fetch HTML via the best available method.
	var rawHTML string
	var err error

	if hasBrowser {
		rawHTML, err = fetchRenderedHTML(pageURL)
	} else {
		log.Printf("[fallback] crawling via native HTTP: %s", pageURL)
		rawHTML, err = fetchPlainHTTP(pageURL)
	}
	if err != nil {
		return CrawledPage{}, err
	}

	// Stage 3: extract clean article text.
	return extractMainText(pageURL, rawHTML)
}

// ─────────────────────────────────────────────
// Step 4: Synthesise & Stream Final Answer
// ─────────────────────────────────────────────

// buildSafeContext implements Budget-Aware Context Building.
func buildSafeContext(results []SearchResult, crawled []CrawledPage, maxChars int) string {
	var sb strings.Builder

	// 1. Always include Search Snippets (Essential metadata)
	sb.WriteString("# SEARCH RESULTS\n")
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("[%d] %s: %s\n", i+1, r.Title, r.Snippet))
	}

	// 2. Conditionally add Crawled Pages until budget is exhausted
	sb.WriteString("\n# PAGE CONTENT\n")
	for _, page := range crawled {
		entry := fmt.Sprintf("Source [%s]: %s\n", page.URL, sanitizeText(page.Content))

		// Check if adding this page exceeds our char-limit (proxy for tokens)
		if sb.Len()+len(entry) > maxChars {
			sb.WriteString("\n[Remaining sources omitted to fit context window]")
			break
		}
		sb.WriteString(entry)
	}
	return sb.String()
}

// streamAnswer calls the LLM with the assembled context and streams the
// response token-by-token to the provided writer. Sources are cited inline
// using [N] notation matching the result list appended at the end.
func streamAnswer(userQuery, context string, results []SearchResult, out io.Writer) error {
	systemPrompt := `You are a knowledgeable assistant with access to real-time web search results.
Answer the user's question using the provided context.
Cite your sources inline using [N] notation corresponding to the search result numbers.
Be accurate, concise, and helpful. If the context does not contain enough information, say so.`

	userPrompt := fmt.Sprintf("Context:\n%s\n\nQuestion: %s", context, userQuery)

	reqBody := LLMRequest{
		Model: searchLLMModel(),
		Messages: []LLMMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature: 0.7,
		Stream:      true,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshalling stream request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, searchLLMBaseURL(), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("building stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+searchLLMAPIKey())
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stream HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Read Server-Sent Events line by line.
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var chunk LLMStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // skip malformed chunks
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				fmt.Fprint(out, choice.Delta.Content)
			}
		}
	}

	// Append numbered source list at the end of the streamed response.
	fmt.Fprintln(out, "\n\n---\n**Sources:**")
	for i, r := range results {
		fmt.Fprintf(out, "[%d] %s – %s\n", i+1, r.Title, r.URL)
	}

	return scanner.Err()
}

// ─────────────────────────────────────────────
// Internal LLM Helper (non-streaming)
// ─────────────────────────────────────────────

// callLLM sends a non-streaming request to the LLM and returns the
// full text content of the first choice.
func callLLM(reqBody LLMRequest) (string, error) {
	reqBody.Stream = false

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshalling LLM request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, searchLLMBaseURL(), bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("building LLM request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+searchLLMAPIKey())

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("LLM HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading LLM response: %w", err)
	}

	// Parse OpenAI-compatible response envelope.
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("parsing LLM envelope: %w\nBody: %s", err, string(body))
	}
	if envelope.Error != nil {
		return "", fmt.Errorf("LLM API error: %s", envelope.Error.Message)
	}
	if len(envelope.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}
	return envelope.Choices[0].Message.Content, nil
}

// ─────────────────────────────────────────────
// Public Entry Point
// ─────────────────────────────────────────────

// Search is the top-level pipeline function.
//
//	query      – the user's natural-language question
//	deepCrawl  – if true, fetches and extracts full page content for each result
//	out        – where the streamed final answer is written (e.g. os.Stdout or an http.ResponseWriter)
//	progressCb – callback for progress messages to be streamed as reasoning content
func Search(query string, deepCrawl bool, out io.Writer, progressCb func(string)) error {
	msg := fmt.Sprintf("[search] query=%q deepCrawl=%v hasBrowser=%v", query, deepCrawl, hasBrowser)
	log.Println(msg)
	if progressCb != nil {
		progressCb(msg)
	}

	// ── Step 1: Fetch raw DDG HTML ────────────────────────────────────────
	msg = "[search] fetching DuckDuckGo Lite results…"
	log.Println(msg)
	if progressCb != nil {
		progressCb(msg)
	}
	rawHTML, err := fetchDDGHTML(query)
	if err != nil {
		return fmt.Errorf("step1 DDG fetch: %w", err)
	}

	// ── Step 2: Deterministic Search Parsing ─────────────────────────────
	msg = "[search] extracting results…"
	log.Println(msg)
	if progressCb != nil {
		progressCb(msg)
	}
	results := extractResults(rawHTML)
	if len(results) > defaultTopN {
		results = results[:defaultTopN]
	}
	msg = fmt.Sprintf("[search] extracted %d results", len(results))
	log.Println(msg)
	if progressCb != nil {
		progressCb(msg)
	}

	// ── Step 3  Deep crawl ────────────────────────────────────
	var crawled []CrawledPage
	if deepCrawl {
		msg = "[search] starting deep crawl…"
		log.Println(msg)
		if progressCb != nil {
			progressCb(msg)
		}

		// allowedResults tracks only URLs that passed the pre-flight and were
		// successfully crawled. Blocked or errored entries are excluded so their
		// URLs and text never appear in citations or the LLM context.
		var allowedResults []SearchResult

		for _, r := range results {
			if r.URL == "" {
				continue
			}
			msg := fmt.Sprintf("[search] crawling %s", r.URL)
			log.Println(msg)
			if progressCb != nil {
				progressCb(msg)
			}
			page, err := crawlURL(r.URL)
			if err != nil {
				var cfErr *cfBlockedError
				if errors.As(err, &cfErr) {
					log.Printf("[search] skipping blocked URL %s: %s", r.URL, cfErr.Reason)
					continue // drop entirely – no URL or text leaks into output
				}
				log.Printf("[search] crawl error for %s: %v (skipping)", r.URL, err)
				continue
			}
			allowedResults = append(allowedResults, r)
			crawled = append(crawled, page)
		}

		// Replace with the clean set so citations only reference reachable sources.
		results = allowedResults
		msg = fmt.Sprintf("[search] crawled %d/%d pages", len(crawled), defaultTopN)
		log.Println(msg)
		if progressCb != nil {
			progressCb(msg)
		}
	}

	// ── Step 4: Synthesise & stream ───────────────────────────────────────
	msg = "[search] streaming final answer…"
	log.Println(msg)
	if progressCb != nil {
		progressCb(msg)
	}
	ctx := buildSafeContext(results, crawled, 32000)
	if err := streamAnswer(query, ctx, results, out); err != nil {
		return fmt.Errorf("step4 stream: %w", err)
	}

	return nil
}

// ─────────────────────────────────────────────
// Multi-Query / Query Expansion
// ─────────────────────────────────────────────

type SearchQueryGen struct {
	Queries []string `json:"queries"`
}

func generateSearchQueries(ctx context.Context, messages []map[string]any, model, apiKey, baseURL string) ([]string, error) {
	systemPrompt := `You are an expert web search query generator. 
Given the user's conversation history, generate up to 3 distinct search queries to gather the necessary information to answer the user's final prompt. 
Output ONLY valid JSON in this format: {"queries": ["query1", "query2"]}`

	var promptMessages []LLMMessage
	promptMessages = append(promptMessages, LLMMessage{Role: "system", Content: systemPrompt})

	// Add history, keeping only 'role' and 'content' strings
	for _, m := range messages {
		if role, ok := m["role"].(string); ok {
			var contentStr string
			switch content := m["content"].(type) {
			case string:
				contentStr = content
			case []interface{}:
				for _, part := range content {
					if pMap, ok := part.(map[string]interface{}); ok {
						if pMap["type"] == "text" {
							if text, ok := pMap["text"].(string); ok {
								contentStr += text
							}
						}
					}
				}
			}
			promptMessages = append(promptMessages, LLMMessage{Role: role, Content: contentStr})
		}
	}

	reqBody := LLMRequest{
		Model:       model,
		Messages:    promptMessages,
		Temperature: 0.1,
		Stream:      false,
	}

	raw, err := callLLM(reqBody)
	if err != nil {
		return nil, fmt.Errorf("generate queries callLLM: %w", err)
	}

	content := strings.TrimSpace(raw)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var result SearchQueryGen
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("failed to parse queries: %v\nRaw: %s", err, content)
	}

	return result.Queries, nil
}

func executeParallelSearches(queries []string, deepCrawl bool, progressCb func(string)) string {
	var wg sync.WaitGroup
	var mu sync.Mutex

	var allResults []SearchResult
	var allCrawled []CrawledPage
	seenURLs := make(map[string]bool)

	for i, q := range queries {
		wg.Add(1)
		go func(query string, delay int) {
			defer wg.Done()

			// Jitter delay to avoid 429
			time.Sleep(time.Duration(delay*500) * time.Millisecond)

			if progressCb != nil {
				progressCb(fmt.Sprintf("[search] Query %d: %q", delay+1, query))
			}

			if progressCb != nil {
				progressCb(fmt.Sprintf("[search][%d] fetching DuckDuckGo results...", delay+1))
			}
			rawHTML, err := fetchDDGHTML(query)

			if err != nil {
				if progressCb != nil {
					progressCb(fmt.Sprintf("[search][%d] error fetching results: %v", delay+1, err))
				}
				return
			}

			if progressCb != nil {
				progressCb(fmt.Sprintf("[search][%d] extracting results...", delay+1))
			}
			results := extractResults(rawHTML)
			if len(results) > 3 {
				results = results[:3]
			}

			if progressCb != nil {
				progressCb(fmt.Sprintf("[search][%d] found %d results", delay+1, len(results)))
			}

			var localCrawled []CrawledPage
			var localResults []SearchResult

			for _, r := range results {
				mu.Lock()
				if seenURLs[r.URL] {
					mu.Unlock()
					continue
				}
				seenURLs[r.URL] = true
				mu.Unlock()

				localResults = append(localResults, r)

				if deepCrawl {
					if progressCb != nil {
						progressCb(fmt.Sprintf("[search][%d] crawling: %s", delay+1, r.URL))
					}
					page, err := crawlURL(r.URL)
					if err != nil {
						if progressCb != nil {
							progressCb(fmt.Sprintf("[search][%d] error crawling %s: %v", delay+1, r.URL, err))
						}
						continue
					}
					localCrawled = append(localCrawled, page)
				}
			}

			mu.Lock()
			allResults = append(allResults, localResults...)
			allCrawled = append(allCrawled, localCrawled...)
			mu.Unlock()
		}(q, i)
	}

	wg.Wait()

	return buildSafeContext(allResults, allCrawled, 32000)
}
