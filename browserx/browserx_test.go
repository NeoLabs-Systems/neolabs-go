package browserx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func chromePath() string {
	for _, p := range []string{
		os.Getenv("CHROME_BIN"),
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
	} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startChrome launches a headless Chrome with a throwaway profile and returns
// its DevTools endpoint; it skips the test when no Chrome is installed.
func startChrome(t *testing.T) string {
	t.Helper()
	exe := chromePath()
	if exe == "" {
		t.Skip("no Chrome/Chromium installed")
	}
	port := freePort(t)
	// Chrome's helper processes keep writing the profile after the main
	// process exits, so the profile is removed best-effort, not via t.TempDir.
	profile, err := os.MkdirTemp("", "browserx-profile-")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "--headless=new", "--no-first-run", "--no-default-browser-check",
		"--disable-gpu", "--user-data-dir="+profile, fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-allow-origins=*", "about:blank")
	if err := cmd.Start(); err != nil {
		t.Skipf("chrome did not start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(profile)
	})
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	ok, _ := WaitForEndpoint(context.Background(), endpoint, 20*time.Second)
	if !ok {
		t.Skip("chrome DevTools endpoint did not come up")
	}
	return endpoint
}

const formHTML = `<!doctype html><html><head><title>Form page</title></head><body>
<h1>Hello</h1>
<input id="name" type="text" value="old">
<input id="email" type="email">
<input id="ro" readonly value="x">
<button id="go" onclick="document.getElementById('out').textContent='clicked:'+document.getElementById('name').value">Go now</button>
<button class="dup">A</button><button class="dup">B</button>
<div id="out"></div>
<div id="later" style="display:none">late</div>
<script>setTimeout(()=>{document.getElementById('later').style.display='block'}, 400)</script>
<a href="/other">Other link</a>
<div style="height:3000px">tall</div>
<div id="bottom">bottom</div>
</body></html>`

func newServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc", Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(formHTML))
	})
	return httptest.NewServer(mux)
}

func TestBrowserEndToEnd(t *testing.T) {
	endpoint := startChrome(t)
	srv := newServer()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := Connect(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pages, err := b.Pages(ctx)
	if err != nil || len(pages) == 0 {
		t.Fatalf("pages = %v, %v", pages, err)
	}
	p := pages[0]

	status, err := p.Goto(ctx, srv.URL+"/", GotoOptions{WaitUntil: "domcontentloaded"})
	if err != nil || status != 200 {
		t.Fatalf("goto = %d, %v", status, err)
	}
	if status, _ := p.Goto(ctx, srv.URL+"/missing", GotoOptions{}); status != 404 {
		t.Fatalf("404 page status = %d", status)
	}
	if _, err := p.Goto(ctx, srv.URL+"/", GotoOptions{WaitUntil: "load"}); err != nil {
		t.Fatal(err)
	}
	if u, _ := p.URL(ctx); u != srv.URL+"/" {
		t.Fatalf("url = %q", u)
	}
	if title, _ := p.Title(ctx); title != "Form page" {
		t.Fatalf("title = %q", title)
	}
	html, _ := p.Content(ctx)
	if !strings.HasPrefix(html, "<!DOCTYPE html><html>") || !strings.Contains(html, "<h1>Hello</h1>") {
		t.Fatalf("content = %.60q", html)
	}

	t.Run("navigation errors", func(t *testing.T) {
		dead := fmt.Sprintf("http://127.0.0.1:%d/", freePort(t))
		_, err := p.Goto(ctx, dead, GotoOptions{})
		if err == nil || !strings.Contains(err.Error(), "net::ERR_CONNECTION_REFUSED at "+dead) {
			t.Fatalf("err = %v", err)
		}
		if _, err := p.Goto(ctx, srv.URL+"/", GotoOptions{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("eval", func(t *testing.T) {
		var n int
		if err := p.EvalJSON(ctx, `1+2`, &n); err != nil || n != 3 {
			t.Fatalf("eval = %d, %v", n, err)
		}
		var s string
		if err := p.EvalJSON(ctx, `Promise.resolve('async')`, &s); err != nil || s != "async" {
			t.Fatalf("promise = %q, %v", s, err)
		}
		_, err := p.Eval(ctx, `nope.x`)
		var ee *ExceptionError
		if !asErr(err, &ee) || !strings.HasPrefix(ee.Message, "ReferenceError: nope is not defined") {
			t.Fatalf("exception = %#v", err)
		}
	})

	t.Run("selectors", func(t *testing.T) {
		count := func(sel string) int {
			var n int
			if err := p.EvalWithSelectors(ctx, `return L.queryAll(args[0]).length`, &n, sel); err != nil {
				t.Fatal(err)
			}
			return n
		}
		for sel, want := range map[string]int{
			"#name": 1, "button": 3, "button.dup": 2, "button:has-text('go')": 1, `button:text-is("A")`: 1,
			`text=Other link`: 1, `text="Other"`: 0, `xpath=//h1`: 1, `//h1`: 1, "css=input >> nth=0": 1, "button >> nth=-1": 1,
			"body >> h1": 1, "#later:visible": 0, "id=name": 1,
		} {
			if got := count(sel); got != want {
				t.Errorf("count(%q) = %d, want %d", sel, got, want)
			}
		}
	})

	t.Run("wait for selector", func(t *testing.T) {
		if err := p.WaitForSelector(ctx, "#later", 5*time.Second); err != nil {
			t.Fatal(err)
		}
		err := p.WaitForSelector(ctx, "#nothing", 300*time.Millisecond)
		var te *TimeoutError
		if !asErr(err, &te) || err.Error() != "page.waitForSelector: Timeout 300ms exceeded." {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("fill and type", func(t *testing.T) {
		if err := p.Fill(ctx, "#name", "", 0); err != nil {
			t.Fatal(err)
		}
		if err := p.PressSequentially(ctx, "#name", "Hello wörld!", 0, 0); err != nil {
			t.Fatal(err)
		}
		var v string
		_ = p.EvalJSON(ctx, `document.getElementById('name').value`, &v)
		if v != "Hello wörld!" {
			t.Fatalf("value = %q", v)
		}
		if err := p.Fill(ctx, "#email", "a@b.c", 0); err != nil {
			t.Fatal(err)
		}
		_ = p.EvalJSON(ctx, `document.getElementById('email').value`, &v)
		if v != "a@b.c" {
			t.Fatalf("email = %q", v)
		}
		// Replacing selects the old value first.
		if err := p.Fill(ctx, "#email", "x@y.z", 0); err != nil {
			t.Fatal(err)
		}
		_ = p.EvalJSON(ctx, `document.getElementById('email').value`, &v)
		if v != "x@y.z" {
			t.Fatalf("refilled = %q", v)
		}
		var se *StrictError
		if err := p.Fill(ctx, "button.dup", "x", 0); !asErr(err, &se) || se.Count != 2 {
			t.Fatalf("strict err = %v", err)
		}
		err := p.Fill(ctx, "#ro", "y", 400*time.Millisecond)
		var te *TimeoutError
		if !asErr(err, &te) || te.Op != "locator.fill" {
			t.Fatalf("readonly fill = %v", err)
		}
		if err := p.Fill(ctx, "h1", "y", 0); err == nil || !strings.Contains(err.Error(), "is not an <input>") {
			t.Fatalf("h1 fill = %v", err)
		}
	})

	t.Run("mouse click and keyboard", func(t *testing.T) {
		box, found, err := p.ScrollAndBox(ctx, "#go")
		if err != nil || !found || box == nil || box.Width <= 0 {
			t.Fatalf("box = %v %v %v", box, found, err)
		}
		if err := p.Mouse.Move(ctx, box.X+box.Width/2, box.Y+box.Height/2); err != nil {
			t.Fatal(err)
		}
		if err := p.Mouse.Down(ctx); err != nil {
			t.Fatal(err)
		}
		if err := p.Mouse.Up(ctx); err != nil {
			t.Fatal(err)
		}
		var out string
		_ = p.EvalJSON(ctx, `document.getElementById('out').textContent`, &out)
		if out != "clicked:Hello wörld!" {
			t.Fatalf("out = %q", out)
		}

		if _, found, _ := p.ScrollAndBox(ctx, "#missing"); found {
			t.Fatal("missing element found")
		}
		// Hidden elements have no box.
		hidden, found, _ := p.ScrollAndBoxExpr(ctx, `document.createElement('div')`)
		if !found || hidden != nil {
			t.Fatalf("detached element = %v %v", hidden, found)
		}

		// Wheel scrolls the page; Control+A selects the field text.
		_ = p.Mouse.Move(ctx, 200, 200)
		if err := p.Mouse.Wheel(ctx, 0, 400); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		var y float64
		_ = p.EvalJSON(ctx, `window.scrollY`, &y)
		if y <= 0 {
			t.Fatalf("scrollY = %v", y)
		}
		if err := p.PressOn(ctx, "#name", "ControlOrMeta+A", 0); err != nil {
			t.Fatal(err)
		}
		var sel string
		_ = p.EvalJSON(ctx, `(() => { const e = document.getElementById('name'); return e.value.slice(e.selectionStart, e.selectionEnd) })()`, &sel)
		if sel != "Hello wörld!" {
			t.Fatalf("selection = %q", sel)
		}
		if err := p.Keyboard.Press(ctx, "Nope", 0); err == nil || err.Error() != `Unknown key: "Nope"` {
			t.Fatalf("unknown key err = %v", err)
		}
	})

	t.Run("screenshots", func(t *testing.T) {
		png, err := p.Screenshot(ctx, ScreenshotOptions{})
		if err != nil || len(png) < 100 || string(png[1:4]) != "PNG" {
			t.Fatalf("png = %d bytes, %v", len(png), err)
		}
		jpg, err := p.Screenshot(ctx, ScreenshotOptions{Format: "jpeg", Quality: 70})
		if err != nil || jpg[0] != 0xff || jpg[1] != 0xd8 {
			t.Fatalf("jpeg err = %v", err)
		}
		el, err := p.Screenshot(ctx, ScreenshotOptions{Selector: "h1"})
		if err != nil || len(el) == 0 || len(el) >= len(png) {
			t.Fatalf("element shot = %d bytes (viewport %d), %v", len(el), len(png), err)
		}
		full, err := p.Screenshot(ctx, ScreenshotOptions{FullPage: true})
		if err != nil || len(full) <= len(png) {
			t.Fatalf("full page = %d bytes (viewport %d), %v", len(full), len(png), err)
		}
	})

	t.Run("cookies and init script", func(t *testing.T) {
		cookies, err := b.Cookies(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var sid *Cookie
		for i := range cookies {
			if cookies[i].Name == "sid" {
				sid = &cookies[i]
			}
		}
		if sid == nil || sid.Value != "abc" || !sid.HTTPOnly || sid.Expires != -1 || sid.SameSite != "None" {
			t.Fatalf("cookie = %+v", sid)
		}
		if err := p.AddInitScript(ctx, `window.__marker = 'ran'`); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Goto(ctx, srv.URL+"/", GotoOptions{}); err != nil {
			t.Fatal(err)
		}
		var m string
		_ = p.EvalJSON(ctx, `window.__marker`, &m)
		if m != "ran" {
			t.Fatalf("marker = %q", m)
		}
	})

	t.Run("new pages", func(t *testing.T) {
		got := make(chan *Page, 1)
		b.OnNewPage(func(np *Page) { got <- np })
		np, err := b.NewPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-got:
		case <-time.After(3 * time.Second):
			// NewPage attaches synchronously, so the event may find it known.
		}
		if _, err := np.Goto(ctx, srv.URL+"/", GotoOptions{WaitUntil: "networkidle"}); err != nil {
			t.Fatal(err)
		}
		if err := np.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if !np.Closed() {
			t.Fatal("page not closed")
		}
	})

	t.Run("reload and load states", func(t *testing.T) {
		if err := p.Reload(ctx, GotoOptions{WaitUntil: "load"}); err != nil {
			t.Fatal(err)
		}
		if err := p.WaitForLoadState(ctx, "networkidle", 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := p.WaitForLoadState(ctx, "domcontentloaded", 0); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEndpointHelpers(t *testing.T) {
	ctx := context.Background()
	if EndpointAvailable(ctx, "http://127.0.0.1:1") {
		t.Fatal("closed port reported available")
	}
	ok, err := WaitForEndpoint(ctx, "http://127.0.0.1:1", 300*time.Millisecond)
	if ok || err != nil {
		t.Fatalf("wait = %v, %v", ok, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := WaitForEndpoint(cctx, "http://127.0.0.1:1", time.Second); err == nil {
		t.Fatal("cancelled wait returned nil")
	}
	if _, err := Connect(ctx, "http://127.0.0.1:1"); err == nil {
		t.Fatal("connect to closed port succeeded")
	}
}

func TestKeyDescriptions(t *testing.T) {
	d, err := describeKey("a", map[string]bool{})
	if err != nil || d.key != "a" || d.code != "KeyA" || d.text != "a" || d.keyCode != 65 {
		t.Fatalf("a = %+v %v", d, err)
	}
	d, _ = describeKey("A", map[string]bool{})
	if d.key != "A" || d.code != "KeyA" || d.text != "A" {
		t.Fatalf("A = %+v", d)
	}
	d, _ = describeKey("KeyA", map[string]bool{"Shift": true})
	if d.key != "A" || d.text != "A" {
		t.Fatalf("shift a = %+v", d)
	}
	d, _ = describeKey("a", map[string]bool{"Control": true})
	if d.text != "" {
		t.Fatalf("ctrl+a keeps text: %+v", d)
	}
	for _, name := range []string{"\n", "\r", "Enter"} {
		d, _ = describeKey(name, map[string]bool{})
		if d.key != "Enter" || d.text != "\r" || d.code != "Enter" {
			t.Fatalf("%q = %+v", name, d)
		}
	}
	d, _ = describeKey("Shift", map[string]bool{})
	if d.code != "ShiftLeft" || d.keyCodeNoLoc != 16 || d.location != 1 {
		t.Fatalf("Shift = %+v", d)
	}
	if got := splitCombo("Control+Shift+A"); strings.Join(got, "|") != "Control|Shift|A" {
		t.Fatalf("combo = %v", got)
	}
	if got := splitCombo("Control++"); strings.Join(got, "|") != "Control|+" {
		t.Fatalf("plus combo = %v", got)
	}
	if HasKey("ö") || !HasKey("a") || !HasKey("ArrowDown") {
		t.Fatal("HasKey")
	}
	if modifierMask(map[string]bool{"Alt": true, "Shift": true}) != 9 {
		t.Fatal("mask")
	}
}

func asErr[T error](err error, target *T) bool { return errors.As(err, target) }
