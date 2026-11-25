package browserx

import (
	"context"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Mouse dispatches mouse events at tracked coordinates, like Playwright's
// page.mouse. It starts at (0, 0).
type Mouse struct {
	page       *Page
	keyboard   *Keyboard
	mu         sync.Mutex
	x, y       float64
	lastButton string
	buttons    map[string]bool
}

func buttonsMask(buttons map[string]bool) int64 {
	var mask int64
	if buttons["left"] {
		mask |= 1
	}
	if buttons["right"] {
		mask |= 2
	}
	if buttons["middle"] {
		mask |= 4
	}
	return mask
}

func forceFor(buttons map[string]bool) float64 {
	if len(buttons) > 0 {
		return 0.5
	}
	return 0
}

func (m *Mouse) dispatch(ctx context.Context, p input.DispatchMouseEventParams) error {
	cctx, cancel := m.page.run(ctx)
	defer cancel()
	_, err := chromedp.Call(cctx, input.DispatchMouseEvent, p)
	return wrapErr(ctx, err)
}

// Move moves the pointer to (x, y).
func (m *Mouse) Move(ctx context.Context, x, y float64) error {
	m.mu.Lock()
	m.x, m.y = x, y
	last, mask, force := m.lastButton, buttonsMask(m.buttons), forceFor(m.buttons)
	m.mu.Unlock()
	return m.dispatch(ctx, input.DispatchMouseEventParams{
		Type: input.DispatchMouseEventTypeMouseMoved, X: x, Y: y, Button: input.MouseButton(last), Buttons: mask,
		Modifiers: modifierMask(m.keyboard.snapshotModifiers()), Force: force,
	})
}

// Down presses the left button at the current position.
func (m *Mouse) Down(ctx context.Context) error {
	m.mu.Lock()
	m.lastButton = "left"
	m.buttons["left"] = true
	x, y, mask, force := m.x, m.y, buttonsMask(m.buttons), forceFor(m.buttons)
	m.mu.Unlock()
	return m.dispatch(ctx, input.DispatchMouseEventParams{
		Type: input.DispatchMouseEventTypeMousePressed, X: x, Y: y, Button: input.MouseButtonLeft, Buttons: mask, ClickCount: 1,
		Modifiers: modifierMask(m.keyboard.snapshotModifiers()), Force: force,
	})
}

// Up releases the left button at the current position.
func (m *Mouse) Up(ctx context.Context) error {
	m.mu.Lock()
	m.lastButton = "none"
	delete(m.buttons, "left")
	x, y, mask := m.x, m.y, buttonsMask(m.buttons)
	m.mu.Unlock()
	return m.dispatch(ctx, input.DispatchMouseEventParams{
		Type: input.DispatchMouseEventTypeMouseReleased, X: x, Y: y, Button: input.MouseButtonLeft, Buttons: mask, ClickCount: 1,
		Modifiers: modifierMask(m.keyboard.snapshotModifiers()),
	})
}

// Wheel scrolls at the current position.
func (m *Mouse) Wheel(ctx context.Context, deltaX, deltaY float64) error {
	m.mu.Lock()
	x, y := m.x, m.y
	m.mu.Unlock()
	// The protocol requires both deltas, zero included, which the typed
	// parameters would omit.
	cctx, cancel := m.page.run(ctx)
	defer cancel()
	_, err := chromedp.Run(cctx, func(c context.Context, t *chromedp.Target) (chromedp.Void, error) {
		return chromedp.Void{}, t.Call(c, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": x, "y": y, "deltaX": deltaX, "deltaY": deltaY,
			"modifiers": modifierMask(m.keyboard.snapshotModifiers()),
		}, nil)
	})
	return wrapErr(ctx, err)
}

// Keyboard dispatches key events with Playwright's key names and modifier
// bookkeeping.
type Keyboard struct {
	page    *Page
	mu      sync.Mutex
	pressed map[string]bool // modifier keys currently down
	keys    map[string]bool // key codes currently down
}

func (k *Keyboard) snapshotModifiers() map[string]bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]bool, len(k.pressed))
	for key := range k.pressed {
		out[key] = true
	}
	return out
}

func (k *Keyboard) send(ctx context.Context, p input.DispatchKeyEventParams) error {
	cctx, cancel := k.page.run(ctx)
	defer cancel()
	_, err := chromedp.Call(cctx, input.DispatchKeyEvent, p)
	return wrapErr(ctx, err)
}

// Down presses a key without releasing it.
func (k *Keyboard) Down(ctx context.Context, key string) error {
	k.mu.Lock()
	d, err := describeKey(key, k.pressed)
	if err != nil {
		k.mu.Unlock()
		return err
	}
	repeat := k.keys[d.code]
	k.keys[d.code] = true
	if isModifier(d.key) {
		k.pressed[d.key] = true
	}
	mods := modifierMask(k.pressed)
	commands := editingCommands(d.code, k.pressed)
	k.mu.Unlock()
	typ := input.DispatchKeyEventTypeKeyDown
	if d.text == "" {
		typ = input.DispatchKeyEventTypeRawKeyDown
	}
	return k.send(ctx, input.DispatchKeyEventParams{
		Type: typ, Modifiers: mods, WindowsVirtualKeyCode: d.keyCodeNoLoc, Code: d.code, Key: d.key,
		Text: d.text, UnmodifiedText: d.text, AutoRepeat: &repeat, Location: d.location,
		IsKeypad: new(d.location == keypad), Commands: commands,
	})
}

// Up releases a key.
func (k *Keyboard) Up(ctx context.Context, key string) error {
	k.mu.Lock()
	d, err := describeKey(key, k.pressed)
	if err != nil {
		k.mu.Unlock()
		return err
	}
	if isModifier(d.key) {
		delete(k.pressed, d.key)
	}
	delete(k.keys, d.code)
	mods := modifierMask(k.pressed)
	k.mu.Unlock()
	return k.send(ctx, input.DispatchKeyEventParams{
		Type: input.DispatchKeyEventTypeKeyUp, Modifiers: mods, Key: d.key, WindowsVirtualKeyCode: d.keyCodeNoLoc,
		Code: d.code, Location: d.location,
	})
}

// InsertText inserts text as if pasted: no key events are generated.
func (k *Keyboard) InsertText(ctx context.Context, text string) error {
	cctx, cancel := k.page.run(ctx)
	defer cancel()
	_, err := chromedp.Call(cctx, input.InsertText, input.InsertTextParams{Text: text})
	return wrapErr(ctx, err)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Press presses and releases a key or a combination such as "Control+A",
// holding the modifiers in between. delay is the pause between down and up.
func (k *Keyboard) Press(ctx context.Context, combo string, delay time.Duration) error {
	tokens := splitCombo(combo)
	key := tokens[len(tokens)-1]
	for _, t := range tokens[:len(tokens)-1] {
		if err := k.Down(ctx, t); err != nil {
			return err
		}
	}
	if err := k.Down(ctx, key); err != nil {
		return err
	}
	if err := sleepCtx(ctx, delay); err != nil {
		return err
	}
	if err := k.Up(ctx, key); err != nil {
		return err
	}
	for i := len(tokens) - 2; i >= 0; i-- {
		if err := k.Up(ctx, tokens[i]); err != nil {
			return err
		}
	}
	return nil
}

// Type types text one character at a time: characters the US layout has are
// pressed as keys, anything else is inserted. delay pauses before each
// character, except after the last.
func (k *Keyboard) Type(ctx context.Context, text string, delay time.Duration) error {
	for _, r := range text {
		ch := string(r)
		if HasKey(ch) {
			if err := k.Press(ctx, ch, delay); err != nil {
				return err
			}
			continue
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return err
		}
		if err := k.InsertText(ctx, ch); err != nil {
			return err
		}
	}
	return nil
}
