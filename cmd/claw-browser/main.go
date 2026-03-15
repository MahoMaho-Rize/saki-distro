// claw-browser is an MCP Server that provides headless browser control tools.
// It manages a single headless Chromium instance via CDP (Chrome DevTools Protocol)
// and exposes navigation, screenshot, DOM interaction, and JS evaluation as MCP tools.
//
// Tools:
//   - browser_navigate: navigate to a URL, optionally wait for selector
//   - browser_screenshot: capture viewport or full-page screenshot (base64 PNG)
//   - browser_click: click an element by CSS selector
//   - browser_type: type text into an element by CSS selector
//   - browser_evaluate: evaluate JavaScript and return the result
//   - browser_content: get the page's text content (innerText of body)
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"claw-distro/internal/mcpserver"
)

const (
	defaultNavTimeout = 30 * time.Second
	defaultTimeout    = 15 * time.Second
	maxContentLen     = 100000 // 100KB text content cap
)

func main() {
	addr := os.Getenv("CLAW_BROWSER_ADDR")
	if addr == "" {
		addr = ":9103"
	}

	srv := mcpserver.New("claw-browser", "0.1.0")
	b := newBrowser()
	registerTools(srv, b)

	err := srv.ListenAndServe(addr)
	b.close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claw-browser: %v\n", err)
		os.Exit(1)
	}
}

// browser manages a shared headless Chromium instance.
type browser struct {
	mu        sync.Mutex
	allocCtx  context.Context
	allocCanc context.CancelFunc
	ctx       context.Context
	cancel    context.CancelFunc
}

func newBrowser() *browser {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.WindowSize(1280, 720),
	)

	allocCtx, allocCanc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)

	return &browser{
		allocCtx:  allocCtx,
		allocCanc: allocCanc,
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (b *browser) close() {
	b.cancel()
	b.allocCanc()
}

// run executes chromedp actions with a mutex (CDP is not concurrent-safe).
// Does NOT wrap b.ctx with WithTimeout — that would kill the browser target
// when the timeout context is cancelled (chromedp treats context cancellation
// as a signal to close the CDP session).
func (b *browser) run(_ time.Duration, actions ...chromedp.Action) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return chromedp.Run(b.ctx, actions...)
}

// checkSelector verifies a CSS selector exists on the page via JS.
// Returns an error if the element is not found. This avoids chromedp's
// WaitVisible which blocks indefinitely on missing selectors.
func (b *browser) checkSelector(sel string) error {
	var exists bool
	err := chromedp.Run(b.ctx, chromedp.Evaluate(
		fmt.Sprintf("document.querySelector(%q) !== null", sel), &exists,
	))
	if err != nil {
		return fmt.Errorf("selector check failed: %w", err)
	}
	if !exists {
		return fmt.Errorf("selector %q not found on page", sel)
	}
	return nil
}

func registerTools(srv *mcpserver.Server, b *browser) {
	srv.AddTool(mcpserver.Tool{
		Name:        "browser_navigate",
		Description: "Navigate to a URL. Optionally wait for a CSS selector to appear.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"url"},
			"properties": map[string]any{
				"url":           map[string]any{"type": "string", "description": "URL to navigate to"},
				"wait_selector": map[string]any{"type": "string", "description": "CSS selector to wait for after navigation"},
			},
		}),
	}, func(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			URL          string `json:"url"`
			WaitSelector string `json:"wait_selector"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}
		if p.URL == "" {
			return mcpserver.ErrorResult("url is required")
		}

		actions := []chromedp.Action{
			chromedp.Navigate(p.URL),
		}
		if p.WaitSelector != "" {
			actions = append(actions, chromedp.WaitVisible(p.WaitSelector))
		}

		if err := b.run(defaultNavTimeout, actions...); err != nil {
			return mcpserver.ErrorResult("navigate failed: " + err.Error())
		}

		// Return current URL (may differ from input after redirects).
		var currentURL string
		if err := b.run(defaultTimeout, chromedp.Location(&currentURL)); err != nil {
			return mcpserver.SuccessResult("navigated (could not read URL)")
		}
		return mcpserver.SuccessResult("navigated to " + currentURL)
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "browser_screenshot",
		Description: "Take a screenshot of the current page. Returns base64-encoded PNG.",
		InputSchema: jsonSchema(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"full_page": map[string]any{"type": "boolean", "description": "Capture full page (default: viewport only)"},
				"selector":  map[string]any{"type": "string", "description": "CSS selector to screenshot (default: full viewport)"},
			},
		}),
	}, func(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			FullPage bool   `json:"full_page"`
			Selector string `json:"selector"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}

		var buf []byte
		var action chromedp.Action

		switch {
		case p.Selector != "":
			action = chromedp.Screenshot(p.Selector, &buf)
		case p.FullPage:
			action = chromedp.FullScreenshot(&buf, 90)
		default:
			action = chromedp.ActionFunc(func(ctx context.Context) error {
				var err error
				buf, err = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(ctx)
				return err
			})
		}

		if err := b.run(defaultTimeout, action); err != nil {
			return mcpserver.ErrorResult("screenshot failed: " + err.Error())
		}

		encoded := base64.StdEncoding.EncodeToString(buf)
		return mcpserver.SuccessResult(fmt.Sprintf("data:image/png;base64,%s", encoded))
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "browser_click",
		Description: "Click an element identified by CSS selector.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"selector"},
			"properties": map[string]any{
				"selector": map[string]any{"type": "string", "description": "CSS selector of element to click"},
			},
		}),
	}, func(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Selector string `json:"selector"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}
		if p.Selector == "" {
			return mcpserver.ErrorResult("selector is required")
		}

		if err := b.checkSelector(p.Selector); err != nil {
			return mcpserver.ErrorResult(err.Error())
		}
		if err := b.run(defaultTimeout, chromedp.Click(p.Selector, chromedp.NodeVisible)); err != nil {
			return mcpserver.ErrorResult("click failed: " + err.Error())
		}
		return mcpserver.SuccessResult("clicked " + p.Selector)
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "browser_type",
		Description: "Type text into an input element identified by CSS selector. Clears existing content first.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"selector", "text"},
			"properties": map[string]any{
				"selector": map[string]any{"type": "string", "description": "CSS selector of input element"},
				"text":     map[string]any{"type": "string", "description": "Text to type"},
			},
		}),
	}, func(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Selector string `json:"selector"`
			Text     string `json:"text"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}
		if p.Selector == "" {
			return mcpserver.ErrorResult("selector is required")
		}

		if err := b.checkSelector(p.Selector); err != nil {
			return mcpserver.ErrorResult(err.Error())
		}
		actions := []chromedp.Action{
			chromedp.Clear(p.Selector),
			chromedp.SendKeys(p.Selector, p.Text),
		}
		if err := b.run(defaultTimeout, actions...); err != nil {
			return mcpserver.ErrorResult("type failed: " + err.Error())
		}
		return mcpserver.SuccessResult("typed into " + p.Selector)
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "browser_evaluate",
		Description: "Evaluate a JavaScript expression in the current page and return the result.",
		InputSchema: jsonSchema(map[string]any{
			"type":     "object",
			"required": []string{"expression"},
			"properties": map[string]any{
				"expression": map[string]any{"type": "string", "description": "JavaScript expression to evaluate"},
			},
		}),
	}, func(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var p struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return mcpserver.ErrorResult("invalid arguments: " + err.Error())
		}
		if p.Expression == "" {
			return mcpserver.ErrorResult("expression is required")
		}

		var result interface{}
		if err := b.run(defaultTimeout, chromedp.Evaluate(p.Expression, &result)); err != nil {
			return mcpserver.ErrorResult("evaluate failed: " + err.Error())
		}

		out, _ := json.Marshal(result)
		return mcpserver.SuccessResult(string(out))
	})

	srv.AddTool(mcpserver.Tool{
		Name:        "browser_content",
		Description: "Get the visible text content of the current page (document.body.innerText).",
		InputSchema: jsonSchema(map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}),
	}, func(_ context.Context, args json.RawMessage) *mcpserver.CallToolResult {
		var text string
		if err := b.run(defaultTimeout, chromedp.InnerHTML("body", &text)); err != nil {
			return mcpserver.ErrorResult("content failed: " + err.Error())
		}
		if len(text) > maxContentLen {
			text = text[:maxContentLen] + "\n...(truncated)"
		}
		return mcpserver.SuccessResult(text)
	})
}

func jsonSchema(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
