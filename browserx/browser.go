// Package browserx drives a Chromium browser over the DevTools protocol, with
// the behaviour of Playwright's page API where callers depend on it:
// auto-waiting for visible/enabled/editable elements, Playwright's key names
// and selector extensions, load states and screenshots. It is built on chromedp
// and needs no Node runtime.
package browserx

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

// Browser is a connection to a running Chromium (its default browser context).
type Browser struct {
	root   context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	pages  map[target.ID]*Page
	onPage func(*Page)
	done   chan struct{}
}

// EndpointAvailable reports whether a DevTools HTTP endpoint such as
// http://127.0.0.1:9222 answers /json/version.
func EndpointAvailable(ctx context.Context, endpoint string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/json/version", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// WaitForEndpoint polls EndpointAvailable until it answers or the timeout
// passes. A cancelled ctx returns its error.
func WaitForEndpoint(ctx context.Context, endpoint string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if EndpointAvailable(ctx, endpoint) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return false, nil
}

// Connect attaches to the browser behind a DevTools endpoint (http://host:port
// or a ws:// URL). It opens no tab. Close detaches without closing the browser.
func Connect(ctx context.Context, endpoint string) (*Browser, error) {
	allocCtx, cancelAlloc := remote.NewAllocator(context.Background(), endpoint)
	root, cancelRoot := chromedp.NewContext(allocCtx)
	cancel := func() { cancelRoot(); cancelAlloc() }

	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	// The first call starts the connection; it must run on the root context
	// itself because the connection lives as long as that context.
	if _, err := chromedp.CallBrowser(root, target.GetTargets, target.GetTargetsParams{}); err != nil {
		cancel()
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, err
	}
	b := &Browser{root: root, cancel: cancel, pages: map[target.ID]*Page{}, done: make(chan struct{})}
	go func() {
		<-root.Done()
		close(b.done)
	}()
	if err := chromedp.ListenBrowser(root, b.handleBrowserEvent); err != nil {
		cancel()
		return nil, err
	}
	if _, err := chromedp.CallBrowser(root, target.SetDiscoverTargets, target.SetDiscoverTargetsParams{Discover: true}); err != nil {
		cancel()
		return nil, err
	}
	return b, nil
}

// Done is closed when the connection to the browser is gone.
func (b *Browser) Done() <-chan struct{} { return b.done }

// Connected reports whether the connection is still usable.
func (b *Browser) Connected() bool {
	select {
	case <-b.done:
		return false
	default:
		return true
	}
}

// Close drops the connection and leaves the browser, and its tabs, running.
func (b *Browser) Close() {
	b.cancel()
	<-b.done
}

// OnNewPage registers a callback for tabs that open after the call (popups,
// target=_blank links, NewPage). The page is attached before fn runs. fn runs
// on its own goroutine.
func (b *Browser) OnNewPage(fn func(*Page)) {
	b.mu.Lock()
	b.onPage = fn
	b.mu.Unlock()
}

// handleBrowserEvent attaches pages that appear later, mirroring Playwright's
// context "page" event.
func (b *Browser) handleBrowserEvent(ev any) {
	created, ok := ev.(*target.EventTargetCreated)
	if !ok || created.TargetInfo == nil || created.TargetInfo.Type != "page" {
		return
	}
	info := created.TargetInfo
	go func() {
		if !b.inDefaultContext(info) {
			return
		}
		p, fresh, err := b.attach(info.TargetID)
		if err != nil || !fresh {
			return
		}
		b.mu.Lock()
		fn := b.onPage
		b.mu.Unlock()
		if fn != nil {
			fn(p)
		}
	}()
}

// inDefaultContext reports whether a tab belongs to the browser's default
// context, as opposed to a context created through the protocol.
func (b *Browser) inDefaultContext(info *target.Info) bool {
	if info.BrowserContextID == "" {
		return true
	}
	res, err := chromedp.CallBrowser(b.root, target.GetBrowserContexts, cdp.Empty{})
	if err != nil {
		return false
	}
	for _, id := range res.BrowserContextIDs {
		if id == info.BrowserContextID {
			return false
		}
	}
	return true
}

// attach returns the Page for a target, creating it on first sight. fresh is
// true when this call created it.
func (b *Browser) attach(id target.ID) (p *Page, fresh bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.pages[id]; ok && !p.Closed() {
		return p, false, nil
	}
	p, err = newPage(b, id)
	if err != nil {
		return nil, false, err
	}
	b.pages[id] = p
	return p, true, nil
}

// Pages lists the open tabs of the default browser context in the order the
// browser reports them.
func (b *Browser) Pages(ctx context.Context) ([]*Page, error) {
	cctx, cancel := mergeContext(b.root, ctx)
	defer cancel()
	res, err := chromedp.CallBrowser(cctx, target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		return nil, wrapErr(ctx, err)
	}
	var pages []*Page
	for _, info := range res.TargetInfos {
		if info.Type != "page" || !b.inDefaultContext(info) {
			continue
		}
		p, _, err := b.attach(info.TargetID)
		if err != nil {
			continue
		}
		pages = append(pages, p)
	}
	return pages, nil
}

// NewPage opens a blank tab.
func (b *Browser) NewPage(ctx context.Context) (*Page, error) {
	cctx, cancel := mergeContext(b.root, ctx)
	defer cancel()
	res, err := chromedp.CallBrowser(cctx, target.CreateTarget, target.CreateTargetParams{URL: "about:blank"})
	if err != nil {
		return nil, wrapErr(ctx, err)
	}
	p, _, err := b.attach(res.TargetID)
	return p, err
}

// Cookies returns every cookie of the default browser context, shaped like
// Playwright's context.cookies().
func (b *Browser) Cookies(ctx context.Context) ([]Cookie, error) {
	cctx, cancel := mergeContext(b.root, ctx)
	defer cancel()
	res, err := chromedp.CallBrowser(cctx, storage.GetCookies, storage.GetCookiesParams{})
	if err != nil {
		return nil, wrapErr(ctx, err)
	}
	cookies := make([]Cookie, 0, len(res.Cookies))
	for _, c := range res.Cookies {
		sameSite := string(c.SameSite)
		if sameSite == "" {
			sameSite = "None"
		}
		expires := c.Expires
		if c.Session {
			expires = -1
		}
		cookies = append(cookies, Cookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Expires: expires, HTTPOnly: c.HTTPOnly, Secure: c.Secure, SameSite: sameSite,
		})
	}
	return cookies, nil
}

// Cookie is the cookie shape Playwright returns, in its key order.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"`
}

// mergeContext returns a child of parent that is also cancelled with caller.
func mergeContext(parent, caller context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(caller, cancel)
	return ctx, func() { stop(); cancel() }
}

// wrapErr reports the caller's own cancellation or deadline ahead of the
// protocol error it caused.
func wrapErr(caller context.Context, err error) error {
	if cerr := caller.Err(); cerr != nil {
		return cerr
	}
	return err
}
