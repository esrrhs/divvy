package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// BrowserClient drives a single headless Chrome tab shared across leaves.
// Like WebClient it is nil-safe: a Sandbox without one reports the browser
// tools disabled. All Chrome actions are serialized because one tab is used.
type BrowserClient struct {
	mu sync.Mutex

	allocCancel context.CancelFunc
	ctx         context.Context // long-lived browser + tab context
	cancel      context.CancelFunc
	started     bool
}

// NewBrowserClient verifies a Chrome/Chromium binary exists and builds a
// headless allocator. The browser process starts lazily on the first action.
func NewBrowserClient() (*BrowserClient, error) {
	if _, err := findChrome(); err != nil {
		return nil, err
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		// /dev/shm is tiny (64MB) on container/CI runners, which makes
		// Chrome hang during startup; use a temp dir for shared memory.
		chromedp.Flag("disable-dev-shm-usage", true),
		// Cold starts on busy CI runners can take longer than chromedp's
		// 20s default, surfacing as "websocket url timeout reached".
		chromedp.WSURLReadTimeout(60*time.Second),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	return &BrowserClient{allocCancel: allocCancel, ctx: allocCtx}, nil
}

// ensure creates the browser context once. The browser/target launches on
// the first real Run. That context is never canceled between actions (which
// would close the target), only on Close or user interrupt.
func (b *BrowserClient) ensure() error {
	if b.started {
		return nil
	}
	b.ctx, b.cancel = chromedp.NewContext(b.ctx)
	b.started = true
	return nil
}

// Close stops the browser and its allocator.
func (b *BrowserClient) Close() {
	if b.cancel != nil {
		b.cancel()
	}
	b.allocCancel()
}

// Navigate loads url (waiting for load) and returns title plus rendered text.
func (b *BrowserClient) Navigate(ctx context.Context, rawURL string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensure(); err != nil {
		return "", err
	}
	var title, body string
	err := b.run(ctx,
		chromedp.Navigate(rawURL),
		chromedp.Title(&title),
		chromedp.Text("body", &body, chromedp.NodeVisible, chromedp.ByQuery),
	)
	if err != nil {
		return "", err
	}
	out := "Title: " + title + "\n" + collapseWS(body)
	if len(out) > 6000 {
		out = out[:6000] + "\n[truncated]"
	}
	return out, nil
}

// Click clicks the first element matching selector.
func (b *BrowserClient) Click(ctx context.Context, selector string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensure(); err != nil {
		return err
	}
	return b.run(ctx, chromedp.Click(selector, chromedp.NodeVisible, chromedp.ByQuery))
}

// Type sends keystrokes into selector after waiting for it.
func (b *BrowserClient) Type(ctx context.Context, selector, text string, clear bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensure(); err != nil {
		return err
	}
	actions := []chromedp.Action{chromedp.WaitVisible(selector, chromedp.ByQuery)}
	if clear {
		// Select existing contents and delete before typing.
		actions = append(actions,
			chromedp.SendKeys(selector, "\u0001a", chromedp.ByQuery), // Ctrl+A
			chromedp.SendKeys(selector, "\u0008", chromedp.ByQuery),  // Backspace
		)
	}
	actions = append(actions, chromedp.SendKeys(selector, text, chromedp.ByQuery))
	return b.run(ctx, actions...)
}

// Text returns rendered text of selector (or body when selector is empty).
func (b *BrowserClient) Text(ctx context.Context, selector string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensure(); err != nil {
		return "", err
	}
	if strings.TrimSpace(selector) == "" {
		selector = "body"
	}
	var text string
	err := b.run(ctx, chromedp.Text(selector, &text, chromedp.NodeVisible, chromedp.ByQuery))
	return collapseWS(text), err
}

// Screenshot captures the full page as PNG bytes.
func (b *BrowserClient) Screenshot(ctx context.Context) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensure(); err != nil {
		return nil, err
	}
	var buf []byte
	err := b.run(ctx, chromedp.FullScreenshot(&buf, 100)) // 100 → PNG
	return buf, err
}

// run executes actions on the long-lived browser context. The user context
// only cancels on real interruption (then the browser closes); it does not
// get canceled between actions, preserving the target.
func (b *BrowserClient) run(ctx context.Context, actions ...chromedp.Action) error {
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if b.cancel != nil {
				b.cancel()
			}
		case <-finished:
		}
	}()
	err := chromedp.Run(b.ctx, actions...)
	close(finished)
	return err
}

// findChrome locates a usable Chrome/Chromium/Edge binary across platforms.
func findChrome() (string, error) {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
		"/usr/bin/microsoft-edge",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome", "msedge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no Chrome/Chromium found; install Chrome or run without -browser (looked in standard paths and PATH)")
}
