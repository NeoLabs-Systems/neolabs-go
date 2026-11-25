package browserx

import (
	"fmt"
	"runtime"
	"strings"
)

// keyDef is one entry of the US keyboard layout, the same table Playwright
// uses, so key names ("Enter", "Control+A", "ArrowDown", "a") mean the same.
type keyDef struct {
	keyCode  int64
	key      string
	shiftKey string
	text     string
	location int64
	// keyCodeNoLoc is the virtual key code sent to the browser; it differs from
	// keyCode for the left/right modifier keys.
	keyCodeNoLoc int64
	shiftKeyCode int64
}

// keyDescription is what is sent for one key press.
type keyDescription struct {
	key, code, text       string
	keyCode, keyCodeNoLoc int64
	location              int64
	shifted               *keyDescription
}

const keypad = 3

var usLayout = map[string]keyDef{
	"Escape": {keyCode: 27, key: "Escape"},
	"F1":     {keyCode: 112, key: "F1"}, "F2": {keyCode: 113, key: "F2"}, "F3": {keyCode: 114, key: "F3"},
	"F4": {keyCode: 115, key: "F4"}, "F5": {keyCode: 116, key: "F5"}, "F6": {keyCode: 117, key: "F6"},
	"F7": {keyCode: 118, key: "F7"}, "F8": {keyCode: 119, key: "F8"}, "F9": {keyCode: 120, key: "F9"},
	"F10": {keyCode: 121, key: "F10"}, "F11": {keyCode: 122, key: "F11"}, "F12": {keyCode: 123, key: "F12"},
	"Backquote": {keyCode: 192, shiftKey: "~", key: "`"},
	"Digit1":    {keyCode: 49, shiftKey: "!", key: "1"}, "Digit2": {keyCode: 50, shiftKey: "@", key: "2"},
	"Digit3": {keyCode: 51, shiftKey: "#", key: "3"}, "Digit4": {keyCode: 52, shiftKey: "$", key: "4"},
	"Digit5": {keyCode: 53, shiftKey: "%", key: "5"}, "Digit6": {keyCode: 54, shiftKey: "^", key: "6"},
	"Digit7": {keyCode: 55, shiftKey: "&", key: "7"}, "Digit8": {keyCode: 56, shiftKey: "*", key: "8"},
	"Digit9": {keyCode: 57, shiftKey: "(", key: "9"}, "Digit0": {keyCode: 48, shiftKey: ")", key: "0"},
	"Minus": {keyCode: 189, shiftKey: "_", key: "-"}, "Equal": {keyCode: 187, shiftKey: "+", key: "="},
	"Backslash": {keyCode: 220, shiftKey: "|", key: "\\"}, "Backspace": {keyCode: 8, key: "Backspace"},
	"Tab":  {keyCode: 9, key: "Tab"},
	"KeyQ": {keyCode: 81, shiftKey: "Q", key: "q"}, "KeyW": {keyCode: 87, shiftKey: "W", key: "w"},
	"KeyE": {keyCode: 69, shiftKey: "E", key: "e"}, "KeyR": {keyCode: 82, shiftKey: "R", key: "r"},
	"KeyT": {keyCode: 84, shiftKey: "T", key: "t"}, "KeyY": {keyCode: 89, shiftKey: "Y", key: "y"},
	"KeyU": {keyCode: 85, shiftKey: "U", key: "u"}, "KeyI": {keyCode: 73, shiftKey: "I", key: "i"},
	"KeyO": {keyCode: 79, shiftKey: "O", key: "o"}, "KeyP": {keyCode: 80, shiftKey: "P", key: "p"},
	"BracketLeft": {keyCode: 219, shiftKey: "{", key: "["}, "BracketRight": {keyCode: 221, shiftKey: "}", key: "]"},
	"CapsLock": {keyCode: 20, key: "CapsLock"},
	"KeyA":     {keyCode: 65, shiftKey: "A", key: "a"}, "KeyS": {keyCode: 83, shiftKey: "S", key: "s"},
	"KeyD": {keyCode: 68, shiftKey: "D", key: "d"}, "KeyF": {keyCode: 70, shiftKey: "F", key: "f"},
	"KeyG": {keyCode: 71, shiftKey: "G", key: "g"}, "KeyH": {keyCode: 72, shiftKey: "H", key: "h"},
	"KeyJ": {keyCode: 74, shiftKey: "J", key: "j"}, "KeyK": {keyCode: 75, shiftKey: "K", key: "k"},
	"KeyL": {keyCode: 76, shiftKey: "L", key: "l"}, "Semicolon": {keyCode: 186, shiftKey: ":", key: ";"},
	"Quote": {keyCode: 222, shiftKey: `"`, key: "'"}, "Enter": {keyCode: 13, key: "Enter", text: "\r"},
	"ShiftLeft": {keyCode: 160, keyCodeNoLoc: 16, key: "Shift", location: 1},
	"KeyZ":      {keyCode: 90, shiftKey: "Z", key: "z"}, "KeyX": {keyCode: 88, shiftKey: "X", key: "x"},
	"KeyC": {keyCode: 67, shiftKey: "C", key: "c"}, "KeyV": {keyCode: 86, shiftKey: "V", key: "v"},
	"KeyB": {keyCode: 66, shiftKey: "B", key: "b"}, "KeyN": {keyCode: 78, shiftKey: "N", key: "n"},
	"KeyM": {keyCode: 77, shiftKey: "M", key: "m"}, "Comma": {keyCode: 188, shiftKey: "<", key: ","},
	"Period": {keyCode: 190, shiftKey: ">", key: "."}, "Slash": {keyCode: 191, shiftKey: "?", key: "/"},
	"ShiftRight":   {keyCode: 161, keyCodeNoLoc: 16, key: "Shift", location: 2},
	"ControlLeft":  {keyCode: 162, keyCodeNoLoc: 17, key: "Control", location: 1},
	"MetaLeft":     {keyCode: 91, key: "Meta", location: 1},
	"AltLeft":      {keyCode: 164, keyCodeNoLoc: 18, key: "Alt", location: 1},
	"Space":        {keyCode: 32, key: " "},
	"AltRight":     {keyCode: 165, keyCodeNoLoc: 18, key: "Alt", location: 2},
	"AltGraph":     {keyCode: 225, key: "AltGraph"},
	"MetaRight":    {keyCode: 92, key: "Meta", location: 2},
	"ContextMenu":  {keyCode: 93, key: "ContextMenu"},
	"ControlRight": {keyCode: 163, keyCodeNoLoc: 17, key: "Control", location: 2},
	"PrintScreen":  {keyCode: 44, key: "PrintScreen"}, "ScrollLock": {keyCode: 145, key: "ScrollLock"},
	"Pause": {keyCode: 19, key: "Pause"}, "PageUp": {keyCode: 33, key: "PageUp"},
	"PageDown": {keyCode: 34, key: "PageDown"}, "Insert": {keyCode: 45, key: "Insert"},
	"Delete": {keyCode: 46, key: "Delete"}, "Home": {keyCode: 36, key: "Home"}, "End": {keyCode: 35, key: "End"},
	"ArrowLeft": {keyCode: 37, key: "ArrowLeft"}, "ArrowUp": {keyCode: 38, key: "ArrowUp"},
	"ArrowRight": {keyCode: 39, key: "ArrowRight"}, "ArrowDown": {keyCode: 40, key: "ArrowDown"},
	"AudioVolumeMute": {keyCode: 173, key: "AudioVolumeMute"}, "AudioVolumeDown": {keyCode: 174, key: "AudioVolumeDown"},
	"AudioVolumeUp": {keyCode: 175, key: "AudioVolumeUp"}, "MediaTrackNext": {keyCode: 176, key: "MediaTrackNext"},
	"MediaTrackPrevious": {keyCode: 177, key: "MediaTrackPrevious"}, "MediaPlayPause": {keyCode: 179, key: "MediaPlayPause"},
	"NumLock":        {keyCode: 144, key: "NumLock"},
	"NumpadDivide":   {keyCode: 111, key: "/", location: keypad},
	"NumpadMultiply": {keyCode: 106, key: "*", location: keypad},
	"NumpadSubtract": {keyCode: 109, key: "-", location: keypad},
	"Numpad7":        {keyCode: 36, shiftKeyCode: 103, key: "Home", shiftKey: "7", location: keypad},
	"Numpad8":        {keyCode: 38, shiftKeyCode: 104, key: "ArrowUp", shiftKey: "8", location: keypad},
	"Numpad9":        {keyCode: 33, shiftKeyCode: 105, key: "PageUp", shiftKey: "9", location: keypad},
	"Numpad4":        {keyCode: 37, shiftKeyCode: 100, key: "ArrowLeft", shiftKey: "4", location: keypad},
	"Numpad5":        {keyCode: 12, shiftKeyCode: 101, key: "Clear", shiftKey: "5", location: keypad},
	"Numpad6":        {keyCode: 39, shiftKeyCode: 102, key: "ArrowRight", shiftKey: "6", location: keypad},
	"NumpadAdd":      {keyCode: 107, key: "+", location: keypad},
	"Numpad1":        {keyCode: 35, shiftKeyCode: 97, key: "End", shiftKey: "1", location: keypad},
	"Numpad2":        {keyCode: 40, shiftKeyCode: 98, key: "ArrowDown", shiftKey: "2", location: keypad},
	"Numpad3":        {keyCode: 34, shiftKeyCode: 99, key: "PageDown", shiftKey: "3", location: keypad},
	"Numpad0":        {keyCode: 45, shiftKeyCode: 96, key: "Insert", shiftKey: "0", location: keypad},
	"NumpadDecimal":  {keyCode: 46, shiftKeyCode: 110, key: "\x00", shiftKey: ".", location: keypad},
	"NumpadEnter":    {keyCode: 13, key: "Enter", text: "\r", location: keypad},
}

var keyAliases = map[string][]string{
	"ShiftLeft": {"Shift"}, "ControlLeft": {"Control"}, "AltLeft": {"Alt"},
	"MetaLeft": {"Meta"}, "Enter": {"\n", "\r"},
}

// layoutIndex maps every name a key can be pressed by (code, alias, the
// character it types) to its description, built once from usLayout.
var layoutIndex = buildLayout()

func buildLayout() map[string]*keyDescription {
	out := make(map[string]*keyDescription, len(usLayout)*2)
	for code, def := range usLayout {
		d := keyDescription{
			key: def.key, code: code, text: def.text, keyCode: def.keyCode,
			keyCodeNoLoc: def.keyCodeNoLoc, location: def.location,
		}
		if d.keyCodeNoLoc == 0 {
			d.keyCodeNoLoc = def.keyCode
		}
		if len([]rune(def.key)) == 1 {
			d.text = def.key
		}
		var shifted *keyDescription
		if def.shiftKey != "" {
			s := d
			s.key, s.text = def.shiftKey, def.shiftKey
			if def.shiftKeyCode != 0 {
				s.keyCode = def.shiftKeyCode
			}
			shifted = &s
		}
		withShift := d
		withShift.shifted = shifted
		out[code] = &withShift
		for _, alias := range keyAliases[code] {
			plain := d
			out[alias] = &plain
		}
		if def.location != 0 {
			continue
		}
		if len([]rune(d.key)) == 1 {
			plain := d
			out[d.key] = &plain
		}
		if shifted != nil {
			plain := *shifted
			out[shifted.key] = &plain
		}
	}
	return out
}

// HasKey reports whether the US layout can press s as one key (a single
// character or a key name); other text must be inserted instead.
func HasKey(s string) bool {
	_, ok := layoutIndex[s]
	return ok
}

var allModifiers = []string{"Alt", "Control", "Meta", "Shift"}

func isModifier(key string) bool {
	for _, m := range allModifiers {
		if m == key {
			return true
		}
	}
	return false
}

func modifierMask(pressed map[string]bool) int64 {
	var mask int64
	if pressed["Alt"] {
		mask |= 1
	}
	if pressed["Control"] {
		mask |= 2
	}
	if pressed["Meta"] {
		mask |= 4
	}
	if pressed["Shift"] {
		mask |= 8
	}
	return mask
}

// resolveSmartModifier turns ControlOrMeta into the platform's shortcut key.
func resolveSmartModifier(key string) string {
	if key == "ControlOrMeta" {
		if runtime.GOOS == "darwin" {
			return "Meta"
		}
		return "Control"
	}
	return key
}

// describeKey looks a key name up for the current set of pressed modifiers.
func describeKey(name string, pressed map[string]bool) (keyDescription, error) {
	name = resolveSmartModifier(name)
	d, ok := layoutIndex[name]
	if !ok {
		return keyDescription{}, fmt.Errorf("Unknown key: %q", name)
	}
	out := *d
	if pressed["Shift"] && d.shifted != nil {
		out = *d.shifted
	}
	if len(pressed) > 1 || (!pressed["Shift"] && len(pressed) == 1) {
		out.text = ""
	}
	return out, nil
}

// splitCombo splits "Control+Shift+A" into its keys; a literal "+" key stays
// whole ("Control++" is Control and "+").
func splitCombo(combo string) []string {
	var keys []string
	var building strings.Builder
	for _, r := range combo {
		if r == '+' && building.Len() > 0 {
			keys = append(keys, building.String())
			building.Reset()
			continue
		}
		building.WriteRune(r)
	}
	return append(keys, building.String())
}

// macEditingCommands are the editing commands macOS binds to shortcuts. The
// protocol does not deliver OS key bindings, so Chromium needs them named, as
// Playwright does; on other systems the browser handles the keys itself.
var macEditingCommands = map[string]string{
	"Meta+KeyA": "selectAll", "Meta+KeyC": "copy", "Meta+KeyX": "cut", "Meta+KeyV": "paste",
	"Meta+KeyZ": "undo", "Shift+Meta+KeyZ": "redo",
	"Meta+ArrowUp": "moveToBeginningOfDocument", "Meta+ArrowDown": "moveToEndOfDocument",
	"Shift+Meta+ArrowUp":   "moveToBeginningOfDocumentAndModifySelection",
	"Shift+Meta+ArrowDown": "moveToEndOfDocumentAndModifySelection",
	"Meta+ArrowLeft":       "moveToLeftEndOfLine", "Meta+ArrowRight": "moveToRightEndOfLine",
	"Shift+Meta+ArrowLeft":  "moveToLeftEndOfLineAndModifySelection",
	"Shift+Meta+ArrowRight": "moveToRightEndOfLineAndModifySelection",
	"Meta+Backspace":        "deleteToBeginningOfLine",
}

func editingCommands(code string, pressed map[string]bool) []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	var parts []string
	for _, m := range []string{"Shift", "Control", "Alt", "Meta"} {
		if pressed[m] {
			parts = append(parts, m)
		}
	}
	if cmd, ok := macEditingCommands[strings.Join(append(parts, code), "+")]; ok {
		return []string{cmd}
	}
	return nil
}
