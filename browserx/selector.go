package browserx

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// selectorLib is the in-page helper (selector engine and element state checks)
// evaluated as a JavaScript expression that yields the helper object.
//
//go:embed selector.js
var selectorLib string

// Box is an element's bounding box in main-frame viewport coordinates.
type Box struct {
	X, Y, Width, Height float64
}

// StrictError reports a selector that matched several elements where one was
// required, as Playwright's strict mode does.
type StrictError struct {
	Selector string
	Count    int
}

func (e *StrictError) Error() string {
	return fmt.Sprintf("strict mode violation: locator('%s') resolved to %d elements", e.Selector, e.Count)
}

// EvalWithSelectors evaluates the statements in body as an async function
// body with L (the selector helper: queryAll, visible, ...) and the JSON
// encoded args in scope. The body returns the value to report, e.g.
//
//	body: `return L.queryAll(args[0]).length`
func (p *Page) EvalWithSelectors(ctx context.Context, body string, out any, args ...any) error {
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	expr := "(async (L, args) => {" + body + "\n})(" + selectorLib + ", " + string(encoded) + ")"
	return p.EvalJSON(ctx, expr, out)
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ScrollAndBox scrolls the first element matching selector into view (when it
// is not already) and returns its bounding box. found is false when nothing
// matches; box is nil when the element has no layout.
func (p *Page) ScrollAndBox(ctx context.Context, selector string) (box *Box, found bool, err error) {
	return p.ScrollAndBoxExpr(ctx, "L.queryAll("+jsString(selector)+")[0]")
}

// ScrollAndBoxExpr is ScrollAndBox for an element chosen by a JavaScript
// expression (with L in scope) that yields an Element or undefined.
func (p *Page) ScrollAndBoxExpr(ctx context.Context, elementExpr string) (box *Box, found bool, err error) {
	var res struct {
		Found bool `json:"found"`
		Box   *struct {
			X      float64 `json:"x"`
			Y      float64 `json:"y"`
			Width  float64 `json:"width"`
			Height float64 `json:"height"`
		} `json:"box"`
	}
	body := `const el = ` + elementExpr + `;
if (!el) return { found: false, box: null };
if (el.scrollIntoViewIfNeeded) el.scrollIntoViewIfNeeded(true);
return { found: true, box: L.box(el) };`
	if err := p.EvalWithSelectors(ctx, body, &res); err != nil {
		return nil, false, err
	}
	if res.Box != nil {
		box = &Box{X: res.Box.X, Y: res.Box.Y, Width: res.Box.Width, Height: res.Box.Height}
	}
	return box, res.Found, nil
}

// poll runs check until it reports done, with Playwright's retry backoff
// (20, 50, 100, 100, 500 ms), bounded by the timeout.
func (p *Page) poll(ctx context.Context, op string, timeout time.Duration, check func(context.Context) (bool, error)) error {
	tctx, d, cancel := withTimeout(ctx, timeout)
	defer cancel()
	delays := []time.Duration{0, 20, 50, 100, 100, 500}
	for i := 0; ; i++ {
		done, err := check(tctx)
		if err != nil {
			return timeoutErr(ctx, tctx, op, d, err)
		}
		if done {
			return nil
		}
		wait := delays[len(delays)-1]
		if i < len(delays) {
			wait = delays[i]
		}
		if err := sleepCtx(tctx, wait*time.Millisecond); err != nil {
			return timeoutErr(ctx, tctx, op, d, err)
		}
	}
}

// WaitForSelector waits until the first element matching selector is visible.
func (p *Page) WaitForSelector(ctx context.Context, selector string, timeout time.Duration) error {
	return p.poll(ctx, "page.waitForSelector", timeout, func(c context.Context) (bool, error) {
		var ok bool
		err := p.EvalWithSelectors(c, `const el = L.queryAll(args[0])[0]; return !!el && L.visible(el);`, &ok, selector)
		return ok, err
	})
}

// locatorState is the page-side verdict on a single-element locator.
type locatorState struct {
	Count  int    `json:"count"`
	Ready  bool   `json:"ready"`
	Result string `json:"result"`
	Error  string `json:"error"`
}

// strictCheck is shared by the locator actions: exactly one match, and (when
// actionable) visible, enabled and editable.
const strictCheck = `
const matches = L.queryAll(args[0]);
if (matches.length !== 1) return { count: matches.length };
const el = L.retarget(matches[0], true);
if (!el || !el.isConnected) return { count: 1 };`

// Fill sets the value of the single element matching selector with
// Playwright's locator.fill semantics: it waits for the element to be
// attached, visible, enabled and editable, then selects its content and
// replaces it. An empty value clears the field.
func (p *Page) Fill(ctx context.Context, selector, value string, timeout time.Duration) error {
	var needsInput bool
	err := p.poll(ctx, "locator.fill", timeout, func(c context.Context) (bool, error) {
		var st locatorState
		body := strictCheck + `
if (!L.visible(el) || L.disabled(el)) return { count: 1 };
const readonly = L.readonly(el);
if (readonly === 'error') return { count: 1, error: 'Element is not an <input>, <textarea>, <select> or [contenteditable] and does not have a role allowing [aria-readonly]' };
if (readonly) return { count: 1 };
try { return { count: 1, ready: true, result: L.fill(el, args[1]) }; }
catch (e) { return { count: 1, error: String(e.message || e) }; }`
		if err := p.EvalWithSelectors(c, body, &st, selector, value); err != nil {
			return false, err
		}
		if st.Count > 1 {
			return false, &StrictError{Selector: selector, Count: st.Count}
		}
		if st.Error != "" {
			return false, fmt.Errorf("locator.fill: %s", st.Error)
		}
		needsInput = st.Result == "needsinput"
		return st.Ready, nil
	})
	if err != nil {
		return err
	}
	if !needsInput {
		return nil
	}
	if value != "" {
		return p.Keyboard.InsertText(ctx, value)
	}
	return p.Keyboard.Press(ctx, "Delete", 0)
}

// focus waits for exactly one element matching selector and focuses it.
func (p *Page) focus(ctx context.Context, op, selector string, timeout time.Duration) error {
	return p.poll(ctx, op, timeout, func(c context.Context) (bool, error) {
		var st locatorState
		body := strictCheck + `
if (el.focus) el.focus();
return { count: 1, ready: true };`
		if err := p.EvalWithSelectors(c, body, &st, selector); err != nil {
			return false, err
		}
		if st.Count > 1 {
			return false, &StrictError{Selector: selector, Count: st.Count}
		}
		return st.Ready, nil
	})
}

// PressSequentially focuses the single element matching selector and types
// text into it key by key, like locator.pressSequentially.
func (p *Page) PressSequentially(ctx context.Context, selector, text string, delay, timeout time.Duration) error {
	if err := p.focus(ctx, "locator.pressSequentially", selector, timeout); err != nil {
		return err
	}
	return p.Keyboard.Type(ctx, text, delay)
}

// PressOn focuses the single element matching selector and presses a key, like
// locator.press.
func (p *Page) PressOn(ctx context.Context, selector, key string, timeout time.Duration) error {
	if err := p.focus(ctx, "locator.press", selector, timeout); err != nil {
		return err
	}
	return p.Keyboard.Press(ctx, key, 0)
}

// FirstLine returns the first line of an error message, for callers that show
// only the headline of a page-side exception.
func FirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
