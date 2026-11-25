package browserx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// DefaultTimeout is Playwright's default for navigations and actions.
const DefaultTimeout = 30 * time.Second

// TimeoutError reports an operation that did not finish in time, with
// Playwright's wording ("page.goto: Timeout 30000ms exceeded.").
type TimeoutError struct {
	Op      string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s: Timeout %dms exceeded.", e.Op, e.Timeout.Milliseconds())
}

// Page is one tab.
type Page struct {
	browser  *Browser
	id       target.ID
	ctx      context.Context
	cancel   context.CancelFunc
	detached <-chan struct{}

	lc       *lifecycle
	Mouse    *Mouse
	Keyboard *Keyboard
}

func newPage(b *Browser, id target.ID) (*Page, error) {
	ctx, cancel := chromedp.NewContext(b.root, chromedp.WithTargetID(id), chromedp.WithDetachOnCancel())
	p := &Page{browser: b, id: id, ctx: ctx, cancel: cancel, lc: newLifecycle()}
	p.Keyboard = &Keyboard{page: p, pressed: map[string]bool{}, keys: map[string]bool{}}
	p.Mouse = &Mouse{page: p, keyboard: p.Keyboard, buttons: map[string]bool{}, lastButton: "none"}
	// The first call on a page context starts its session, so it has to be the
	// page context itself, not a derived one.
	if err := chromedp.Listen(ctx, p.lc.handle); err != nil {
		cancel()
		return nil, err
	}
	if _, err := chromedp.Call(ctx, page.SetLifecycleEventsEnabled, page.SetLifecycleEventsEnabledParams{Enabled: true}); err != nil {
		cancel()
		return nil, err
	}
	tree, err := chromedp.Call(ctx, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		cancel()
		return nil, err
	}
	p.lc.setMainFrame(tree.FrameTree.Frame.ID)
	if p.detached, err = chromedp.Detached(ctx); err != nil {
		cancel()
		return nil, err
	}
	return p, nil
}

// ID is the DevTools target id of the tab.
func (p *Page) ID() string { return string(p.id) }

// Closed reports whether the tab is gone or the connection dropped.
func (p *Page) Closed() bool {
	select {
	case <-p.detached:
		return true
	case <-p.ctx.Done():
		return true
	default:
		return false
	}
}

// Close closes the tab.
func (p *Page) Close(ctx context.Context) error {
	cctx, cancel := mergeContext(p.browser.root, ctx)
	defer cancel()
	_, err := chromedp.CallBrowser(cctx, target.CloseTarget, target.CloseTargetParams{TargetID: p.id})
	p.cancel()
	return wrapErr(ctx, err)
}

// run derives the context for one protocol call: it ends with the caller's
// context, the tab, or the connection.
func (p *Page) run(ctx context.Context) (context.Context, context.CancelFunc) {
	return mergeContext(p.ctx, ctx)
}

// withTimeout bounds ctx by d (DefaultTimeout when zero) and maps the
// deadline to a TimeoutError for op.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, time.Duration, context.CancelFunc) {
	if d <= 0 {
		d = DefaultTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, d)
	return tctx, d, cancel
}

// timeoutErr converts an expired deadline of tctx into a TimeoutError, unless
// the caller's own ctx ended first.
func timeoutErr(parent, tctx context.Context, op string, d time.Duration, err error) error {
	if err == nil {
		return nil
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return &TimeoutError{Op: op, Timeout: d}
	}
	return err
}

// URL is the committed URL of the tab.
func (p *Page) URL(ctx context.Context) (string, error) {
	cctx, cancel := p.run(ctx)
	defer cancel()
	res, err := chromedp.CallBrowser(cctx, target.GetTargetInfo, target.GetTargetInfoParams{TargetID: p.id})
	if err != nil {
		return "", wrapErr(ctx, err)
	}
	return res.TargetInfo.URL, nil
}

// Title is document.title.
func (p *Page) Title(ctx context.Context) (string, error) {
	var title string
	err := p.EvalJSON(ctx, `document.title`, &title)
	return title, err
}

// Content is the page's HTML including its doctype, like Playwright's
// page.content().
func (p *Page) Content(ctx context.Context) (string, error) {
	var html string
	err := p.EvalJSON(ctx, `(() => {
  let s = '';
  const t = document.doctype;
  if (t) s = '<!DOCTYPE ' + t.name + (t.publicId ? ' PUBLIC "' + t.publicId + '"' : '') +
    (!t.publicId && t.systemId ? ' SYSTEM' : '') + (t.systemId ? ' "' + t.systemId + '"' : '') + '>';
  if (document.documentElement) s += document.documentElement.outerHTML;
  return s;
})()`, &html)
	return html, err
}

// ExceptionError is a JavaScript exception thrown by evaluated code.
type ExceptionError struct{ Message string }

func (e *ExceptionError) Error() string { return e.Message }

// Eval evaluates a JavaScript expression in the page's main world, waits for a
// returned promise, and returns the result by value as JSON. A result of
// undefined comes back as the JSON value null.
func (p *Page) Eval(ctx context.Context, expression string) (json.RawMessage, error) {
	cctx, cancel := p.run(ctx)
	defer cancel()
	res, err := chromedp.Call(cctx, runtime.Evaluate, runtime.EvaluateParams{
		Expression:    expression,
		ReturnByValue: new(true),
		AwaitPromise:  new(true),
	})
	if err != nil {
		return nil, wrapErr(ctx, err)
	}
	if res.ExceptionDetails != nil {
		return nil, &ExceptionError{Message: exceptionText(res.ExceptionDetails)}
	}
	if res.Result == nil || len(res.Result.Value) == 0 {
		return json.RawMessage("null"), nil
	}
	return json.RawMessage(res.Result.Value), nil
}

// EvalJSON is Eval followed by decoding the result into out.
func (p *Page) EvalJSON(ctx context.Context, expression string, out any) error {
	raw, err := p.Eval(ctx, expression)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("browserx: decode evaluation result: %w", err)
	}
	return nil
}

func exceptionText(d *runtime.ExceptionDetails) string {
	if d.Exception != nil && d.Exception.Description != "" {
		return d.Exception.Description
	}
	if d.Exception != nil && len(d.Exception.Value) > 0 {
		var s string
		if json.Unmarshal(d.Exception.Value, &s) == nil {
			return "Error: " + s
		}
	}
	return d.Text
}

// AddInitScript runs source in every document the tab loads from now on,
// before the page's own scripts, and in the document that is already loaded.
func (p *Page) AddInitScript(ctx context.Context, source string) error {
	cctx, cancel := p.run(ctx)
	defer cancel()
	_, err := chromedp.Call(cctx, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{
		Source: source, RunImmediately: new(true),
	})
	return wrapErr(ctx, err)
}

// GotoOptions tune Goto. WaitUntil is commit, domcontentloaded, load or
// networkidle (default load, as in Playwright).
type GotoOptions struct {
	WaitUntil string
	Timeout   time.Duration
	Referer   string
}

// Goto navigates and waits for the load state, returning the main document's
// HTTP status (0 for a navigation without a network response, such as a hash
// change or about:blank).
func (p *Page) Goto(ctx context.Context, url string, o GotoOptions) (int, error) {
	tctx, d, cancel := withTimeout(ctx, o.Timeout)
	defer cancel()
	status, err := p.goto_(tctx, url, o)
	return status, timeoutErr(ctx, tctx, "page.goto", d, err)
}

func (p *Page) goto_(ctx context.Context, url string, o GotoOptions) (int, error) {
	cctx, cancel := p.run(ctx)
	defer cancel()
	res, err := chromedp.Call(cctx, page.Navigate, page.NavigateParams{URL: url, Referrer: o.Referer})
	if err != nil {
		return 0, err
	}
	if res.ErrorText != "" {
		return 0, fmt.Errorf("%s at %s", res.ErrorText, url)
	}
	if res.LoaderID == "" {
		return 0, nil
	}
	if state := loadStateEvent(o.WaitUntil); state != "" {
		if err := p.lc.wait(ctx, state, res.LoaderID, 0); err != nil {
			return 0, err
		}
	}
	return int(p.lc.statusOf(res.LoaderID)), nil
}

// Reload reloads the tab and waits like Goto.
func (p *Page) Reload(ctx context.Context, o GotoOptions) error {
	tctx, d, cancel := withTimeout(ctx, o.Timeout)
	defer cancel()
	err := func() error {
		epoch := p.lc.currentEpoch()
		cctx, cancel := p.run(tctx)
		defer cancel()
		if _, err := chromedp.Call(cctx, page.Reload, page.ReloadParams{}); err != nil {
			return err
		}
		if state := loadStateEvent(o.WaitUntil); state != "" {
			return p.lc.wait(tctx, state, "", epoch)
		}
		return nil
	}()
	return timeoutErr(ctx, tctx, "page.reload", d, err)
}

// WaitForLoadState waits until the current document reached the state
// (load, domcontentloaded or networkidle).
func (p *Page) WaitForLoadState(ctx context.Context, state string, timeout time.Duration) error {
	tctx, d, cancel := withTimeout(ctx, timeout)
	defer cancel()
	ev := loadStateEvent(state)
	if ev == "" {
		ev = "load"
	}
	err := p.lc.wait(tctx, ev, "", -1)
	if err != nil && ev != "networkIdle" {
		// A document whose lifecycle events were missed (attached after it
		// loaded) is still complete.
		var ready string
		if p.EvalJSON(ctx, `document.readyState`, &ready) == nil &&
			(ready == "complete" || ready == "interactive" && ev == "DOMContentLoaded") {
			return nil
		}
	}
	return timeoutErr(ctx, tctx, "page.waitForLoadState", d, err)
}

// loadStateEvent maps a Playwright load state to Chromium's lifecycle event.
func loadStateEvent(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "commit":
		return ""
	case "domcontentloaded":
		return "DOMContentLoaded"
	case "networkidle":
		return "networkIdle"
	default:
		return "load"
	}
}

// ScreenshotOptions tune Screenshot. Format is "png" (default) or "jpeg".
type ScreenshotOptions struct {
	Format   string
	Quality  int
	FullPage bool
	// Selector captures one element, scrolled into view first.
	Selector string
}

// Screenshot captures the viewport, the full page, or one element. When the
// selector matches nothing the whole page is captured instead.
func (p *Page) Screenshot(ctx context.Context, o ScreenshotOptions) ([]byte, error) {
	params := page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatPng}
	if o.Format == "jpeg" {
		params.Format = page.CaptureScreenshotFormatJpeg
		params.Quality = new(int64(o.Quality))
	}
	cctx, cancel := p.run(ctx)
	defer cancel()
	switch {
	case o.Selector != "":
		box, found, err := p.ScrollAndBox(ctx, o.Selector)
		if err != nil {
			return nil, err
		}
		if found && box != nil {
			params.Clip = &page.Viewport{X: box.X, Y: box.Y, Width: box.Width, Height: box.Height, Scale: 1}
			params.CaptureBeyondViewport = new(true)
		}
		if params.Clip != nil {
			break
		}
		fallthrough
	default:
		if o.FullPage {
			m, err := chromedp.Call(cctx, page.GetLayoutMetrics, cdp.Empty{})
			if err != nil {
				return nil, wrapErr(ctx, err)
			}
			if m.CSSContentSize != nil {
				params.Clip = &page.Viewport{Width: m.CSSContentSize.Width, Height: m.CSSContentSize.Height, Scale: 1}
				params.CaptureBeyondViewport = new(true)
			}
		}
	}
	res, err := chromedp.Call(cctx, page.CaptureScreenshot, params)
	if err != nil {
		return nil, wrapErr(ctx, err)
	}
	return res.Data, nil
}

// lifecycle follows the main frame's load events and document responses.
type lifecycle struct {
	mu        sync.Mutex
	mainFrame cdp.FrameID
	loader    cdp.LoaderID
	names     map[string]bool
	epoch     int
	changed   chan struct{}
	status    map[cdp.LoaderID]int64
	loaders   []cdp.LoaderID
}

func newLifecycle() *lifecycle {
	return &lifecycle{names: map[string]bool{}, changed: make(chan struct{}), status: map[cdp.LoaderID]int64{}}
}

func (l *lifecycle) setMainFrame(id cdp.FrameID) {
	l.mu.Lock()
	l.mainFrame = id
	l.mu.Unlock()
}

// handle receives every event of the tab and must return quickly.
func (l *lifecycle) handle(ev any) {
	switch e := ev.(type) {
	case *page.EventFrameNavigated:
		if e.Frame != nil && e.Frame.ParentID == "" {
			l.setMainFrame(e.Frame.ID)
		}
	case *page.EventLifecycleEvent:
		l.mu.Lock()
		if e.FrameID == l.mainFrame {
			if e.LoaderID != l.loader {
				l.loader, l.names = e.LoaderID, map[string]bool{}
				l.epoch++
			}
			l.names[e.Name] = true
			close(l.changed)
			l.changed = make(chan struct{})
		}
		l.mu.Unlock()
	case *network.EventResponseReceived:
		if e.Type == network.ResourceTypeDocument && e.Response != nil {
			l.mu.Lock()
			if _, seen := l.status[e.LoaderID]; !seen {
				l.loaders = append(l.loaders, e.LoaderID)
				if len(l.loaders) > 32 {
					delete(l.status, l.loaders[0])
					l.loaders = l.loaders[1:]
				}
			}
			l.status[e.LoaderID] = e.Response.Status
			l.mu.Unlock()
		}
	}
}

func (l *lifecycle) currentEpoch() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.epoch
}

func (l *lifecycle) statusOf(loader cdp.LoaderID) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status[loader]
}

// wait blocks until the lifecycle event name was seen. With a loader id the
// event must belong to that loader; with afterEpoch >= 0 it must belong to a
// document that started after that epoch; -1 accepts the current document.
func (l *lifecycle) wait(ctx context.Context, name string, loader cdp.LoaderID, afterEpoch int) error {
	for {
		l.mu.Lock()
		ok := l.names[name]
		switch {
		case loader != "":
			ok = ok && l.loader == loader
		case afterEpoch >= 0:
			ok = ok && l.epoch > afterEpoch
		}
		ch := l.changed
		l.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
