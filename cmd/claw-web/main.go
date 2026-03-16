// claw-web is an MCP Server that provides web search and content fetching tools.
// It exposes web_search (via configurable search API) and web_fetch (URL content
// extraction with readability) as MCP tools.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"regexp"

	"claw-distro/internal/mcpserver"
	"claw-distro/internal/safenet"
)

const (
	maxResponseBytes = 1 << 20 // 1MB max per fetch
	fetchTimeout     = 30 * time.Second
	searchTimeout    = 15 * time.Second
)

var httpClient = &http.Client{
	Timeout: fetchTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

func main() {
	addr := os.Getenv("CLAW_WEB_ADDR")
	if addr == "" {
		addr = ":9102"
	}

	srv := mcpserver.New("claw-web", "0.1.0")
	registerTools(srv)

	if err := srv.ListenAndServe(addr); err != nil {
		fmt.Fprintf(os.Stderr, "claw-web: %v\n", err)
		os.Exit(1)
	}
}

func registerTools(srv *mcpserver.Server) {
	// --- web_fetch: fetch and extract readable content from a URL ---
	srv.AddTool(mcpserver.Tool{
		Name:        "web_fetch",
		Description: "Fetch a URL and extract its readable text content. Returns cleaned text, stripping HTML tags, scripts, and styles.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"url"},
			"properties": map[string]any{
				"url": map[string]any{"type": "string", "description": "URL to fetch"},
			},
		}),
	}, handleWebFetch)

	// --- web_search: search the web via Brave Search API ---
	srv.AddTool(mcpserver.Tool{
		Name:        "web_search",
		Description: "Search the web using Brave Search API. Returns titles, URLs, and snippets for the top results.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"query"},
			"properties": map[string]any{
				"query":       map[string]any{"type": "string", "description": "Search query"},
				"max_results": map[string]any{"type": "integer", "description": "Max results (default 5, max 20)"},
			},
		}),
	}, handleWebSearch)
}

// --- web_fetch handler ---

func handleWebFetch(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpserver.ErrorResult("invalid arguments: " + err.Error())
	}

	if p.URL == "" {
		return mcpserver.ErrorResult("url is required")
	}

	// SSRF prevention: block private IPs, metadata endpoints, DNS rebinding
	if os.Getenv("CLAW_WEB_DISABLE_SSRF_CHECK") != "1" {
		if err := safenet.ValidateFetchURL(p.URL); err != nil {
			return mcpserver.ErrorResult("blocked: " + err.Error())
		}
	}

	req, err := http.NewRequest(http.MethodGet, p.URL, nil)
	if err != nil {
		return mcpserver.ErrorResult(err.Error())
	}
	req.Header.Set("User-Agent", "ClawBot/0.1 (coding agent)")
	req.Header.Set("Accept", "text/html,text/plain,application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return mcpserver.ErrorResult("fetch failed: " + err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return mcpserver.ErrorResult(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return mcpserver.ErrorResult("read failed: " + err.Error())
	}

	contentType := resp.Header.Get("Content-Type")

	// For HTML: extract readable text.
	if strings.Contains(contentType, "text/html") {
		text := extractText(string(body))
		if len(text) > 50000 {
			text = text[:50000] + "\n...(truncated)"
		}
		return mcpserver.SuccessResult(text)
	}

	// For JSON/plain text: return as-is.
	content := string(body)
	if len(content) > 50000 {
		content = content[:50000] + "\n...(truncated)"
	}
	return mcpserver.SuccessResult(content)
}

// Regex patterns for HTML cleaning — compiled once.
var (
	reScript   = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyle    = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	reTag      = regexp.MustCompile(`<[^>]+>`)
	reSpaces   = regexp.MustCompile(`[ \t]+`)
	reNewlines = regexp.MustCompile(`\n{3,}`)
)

// extractText converts HTML to readable plain text by stripping tags.
func extractText(htmlContent string) string {
	s := reScript.ReplaceAllString(htmlContent, "")
	s = reStyle.ReplaceAllString(s, "")
	s = reComment.ReplaceAllString(s, "")
	// Replace block elements with newlines.
	for _, tag := range []string{"</p>", "</div>", "<br>", "<br/>", "<br />", "</h1>", "</h2>", "</h3>", "</li>", "</tr>"} {
		s = strings.ReplaceAll(s, tag, "\n")
	}
	s = reTag.ReplaceAllString(s, "")
	// Decode common HTML entities.
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	// Collapse whitespace.
	s = reSpaces.ReplaceAllString(s, " ")
	s = reNewlines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// --- web_search handler ---

func handleWebSearch(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
	var p struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpserver.ErrorResult("invalid arguments: " + err.Error())
	}
	if p.Query == "" {
		return mcpserver.ErrorResult("query is required")
	}
	if p.MaxResults <= 0 {
		p.MaxResults = 5
	}
	if p.MaxResults > 20 {
		p.MaxResults = 20
	}

	apiKey := os.Getenv("BRAVE_SEARCH_API_KEY")
	if apiKey == "" {
		return mcpserver.ErrorResult("BRAVE_SEARCH_API_KEY not configured")
	}

	searchURL := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d",
		url.QueryEscape(p.Query), p.MaxResults)

	req, err := http.NewRequest(http.MethodGet, searchURL, nil)
	if err != nil {
		return mcpserver.ErrorResult(err.Error())
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)

	client := &http.Client{Timeout: searchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return mcpserver.ErrorResult("search failed: " + err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return mcpserver.ErrorResult(fmt.Sprintf("search API error %d: %s", resp.StatusCode, string(body)))
	}

	var searchResp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&searchResp); err != nil {
		return mcpserver.ErrorResult("parse search results: " + err.Error())
	}

	var buf strings.Builder
	for i, r := range searchResp.Web.Results {
		fmt.Fprintf(&buf, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Description)
	}
	if buf.Len() == 0 {
		return mcpserver.SuccessResult("no results found")
	}
	return mcpserver.SuccessResult(buf.String())
}

func jsonSchema(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
