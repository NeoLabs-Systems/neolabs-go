package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Choice is one option of Select.
type Choice struct {
	Value string
	Label string
	Hint  string
}

func (u *UI) questionLine(message, suffix string) string {
	line := "  " + u.Accent("?") + " " + u.Bold(message)
	if suffix != "" {
		line += " " + u.Muted(suffix)
	}
	return line
}

// lineReader reads answers line by line from the UI's input.
type lineReader struct{ r *bufio.Reader }

func (u *UI) readLine(prompt string) (string, error) {
	fmt.Fprint(u.Out, prompt)
	if u.lines == nil {
		u.lines = &lineReader{bufio.NewReader(u.In)}
	}
	s, err := u.lines.r.ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", ErrCancelled
		}
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// key is one keypress: a name (up, down, return, escape, backspace, ctrl-c,
// ctrl-u, or the character itself) and the text it types.
type key struct{ name, text string }

func parseKeys(b []byte) []key {
	var keys []key
	for len(b) > 0 {
		switch {
		case len(b) >= 3 && b[0] == 0x1b && (b[1] == '[' || b[1] == 'O'):
			name := map[byte]string{'A': "up", 'B': "down", 'C': "right", 'D': "left"}[b[2]]
			i := 2
			for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
				i++
			}
			keys = append(keys, key{name: name})
			b = b[min(i+1, len(b)):]
		case b[0] == 0x1b:
			keys = append(keys, key{name: "escape"})
			b = b[1:]
		case b[0] == '\r' || b[0] == '\n':
			keys = append(keys, key{name: "return"})
			b = b[1:]
		case b[0] == 0x7f || b[0] == 0x08:
			keys = append(keys, key{name: "backspace"})
			b = b[1:]
		case b[0] == 0x03:
			keys = append(keys, key{name: "ctrl-c"})
			b = b[1:]
		case b[0] == 0x15:
			keys = append(keys, key{name: "ctrl-u"})
			b = b[1:]
		case b[0] < 0x20:
			b = b[1:]
		default:
			r := []rune(string(b))
			ch := string(r[0])
			keys = append(keys, key{name: strings.ToLower(ch), text: ch})
			b = b[len(ch):]
		}
	}
	return keys
}

// withKeys reads keypresses until onKey reports it is done. Ctrl+C cancels.
func (u *UI) withKeys(showCursor bool, onKey func(k key) (done bool)) error {
	in := u.In.(*os.File)
	restore, err := keyMode(int(in.Fd()))
	if err != nil {
		return err
	}
	if !showCursor {
		fmt.Fprint(u.Out, "\x1b[?25l")
	}
	defer func() {
		restore()
		fmt.Fprint(u.Out, "\x1b[?25h")
	}()
	buf := make([]byte, 64)
	for {
		n, err := in.Read(buf)
		if err != nil {
			return ErrCancelled
		}
		for _, k := range parseKeys(buf[:n]) {
			if k.name == "ctrl-c" {
				return ErrCancelled
			}
			if onKey(k) {
				return nil
			}
		}
	}
}

// Select is an arrow-key menu that returns the chosen value. Without a
// terminal the same choices become a numbered list read from stdin.
func (u *UI) Select(message string, choices []Choice) (string, error) {
	v, _, err := u.selectChoice(message, choices, 0, false)
	return v, err
}

// SelectOrQuit is Select where q and escape leave the menu; ok is false then.
func (u *UI) SelectOrQuit(message string, choices []Choice) (value string, ok bool, err error) {
	return u.selectChoice(message, choices, 0, true)
}

func (u *UI) selectChoice(message string, choices []Choice, initial int, cancellable bool) (string, bool, error) {
	if !u.Interactive() {
		u.Print(u.questionLine(message, ""))
		for i, c := range choices {
			hint := ""
			if c.Hint != "" {
				hint = " " + u.Muted("— "+c.Hint)
			}
			u.Printf("    %d. %s%s", i+1, c.Label, hint)
		}
		answer, err := u.readLine(fmt.Sprintf("  Choose 1-%d [%d]: ", len(choices), initial+1))
		if err != nil {
			return "", false, err
		}
		index := initial
		if answer != "" {
			if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(choices) {
				index = n - 1
			}
		}
		return choices[index].Value, true, nil
	}

	labelWidth := 0
	for _, c := range choices {
		labelWidth = max(labelWidth, VisibleLength(c.Label))
	}
	labelWidth += 3
	cursor := initial
	var drawn []int
	render := func() {
		u.clearBlock(drawn)
		// Hints share the line only when the window has room for them.
		showHints := u.Width() >= labelWidth+30
		lines := []string{u.questionLine(message, ""), ""}
		for i, c := range choices {
			marker, label := " ", c.Label
			if i == cursor {
				marker, label = u.Accent("›"), u.Command(c.Label)
			}
			hint := ""
			if showHints && c.Hint != "" {
				hint = u.Muted(c.Hint)
			}
			lines = append(lines, strings.TrimRight("  "+marker+" "+PadEnd(label, labelWidth)+hint, " "))
		}
		lines = append(lines, "")
		footer := []string{"↑ ↓ " + u.Muted("move"), "enter " + u.Muted("choose")}
		if cancellable {
			footer = append(footer, "q "+u.Muted("quit"))
		}
		lines = append(lines, "  "+strings.Join(footer, u.Muted("  ·  ")))
		drawn = u.drawBlock(lines)
	}
	render()
	chosen := true
	err := u.withKeys(false, func(k key) bool {
		switch {
		case k.name == "up" || k.name == "k":
			cursor = (cursor - 1 + len(choices)) % len(choices)
		case k.name == "down" || k.name == "j":
			cursor = (cursor + 1) % len(choices)
		case len(k.text) == 1 && k.text[0] >= '1' && k.text[0] <= '9' && int(k.text[0]-'0') <= len(choices):
			cursor = int(k.text[0] - '1')
		case k.name == "return":
			return true
		case cancellable && (k.name == "q" || k.name == "escape"):
			chosen = false
			return true
		default:
			return false
		}
		render()
		return false
	})
	u.clearBlock(drawn)
	if err != nil {
		return "", false, err
	}
	if !chosen {
		return "", false, nil
	}
	u.Print(u.questionLine(message, "") + " " + u.Accent(choices[cursor].Label))
	return choices[cursor].Value, true, nil
}

// Confirm asks a yes/no question; enter takes def.
func (u *UI) Confirm(message string, def bool) (bool, error) {
	suffix := "(Y/n)"
	if !def {
		suffix = "(y/N)"
	}
	if !u.Interactive() {
		answer, err := u.readLine(u.questionLine(message, suffix) + " ")
		if err != nil {
			return false, err
		}
		if answer == "" {
			return def, nil
		}
		return strings.HasPrefix(strings.ToLower(answer), "y"), nil
	}
	fmt.Fprint(u.Out, u.questionLine(message, suffix)+" ")
	answer := def
	err := u.withKeys(false, func(k key) bool {
		switch k.name {
		case "return":
			answer = def
		case "y":
			answer = true
		case "n", "escape":
			answer = false
		default:
			return false
		}
		return true
	})
	if err != nil {
		fmt.Fprintln(u.Out)
		return false, err
	}
	if answer {
		fmt.Fprintln(u.Out, u.Accent("Yes"))
	} else {
		fmt.Fprintln(u.Out, "No")
	}
	return answer, nil
}

// Ask reads a line; an empty answer returns def.
func (u *UI) Ask(message, def string) (string, error) {
	suffix := ""
	if def != "" {
		suffix = "[" + def + "]"
	}
	answer, err := u.readLine(u.questionLine(message, suffix) + " ")
	if err != nil {
		return "", err
	}
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// AskSecret reads a secret; typed characters show as dots. An empty answer
// keeps def.
func (u *UI) AskSecret(message, def string) (string, error) {
	suffix := ""
	if def != "" {
		suffix = "[saved — enter keeps it]"
	}
	if !u.Interactive() {
		answer, err := u.readLine(u.questionLine(message, suffix) + " ")
		if err != nil || answer == "" {
			return def, err
		}
		return answer, nil
	}
	fmt.Fprint(u.Out, u.questionLine(message, suffix)+" ")
	var value []rune
	err := u.withKeys(true, func(k key) bool {
		switch {
		case k.name == "return":
			return true
		case k.name == "backspace":
			if len(value) > 0 {
				value = value[:len(value)-1]
				fmt.Fprint(u.Out, "\b \b")
			}
		case k.name == "ctrl-u":
			fmt.Fprint(u.Out, strings.Repeat("\b \b", len(value)))
			value = nil
		case k.text != "" && k.text >= " ":
			value = append(value, []rune(k.text)...)
			fmt.Fprint(u.Out, strings.Repeat("•", len([]rune(k.text))))
		}
		return false
	})
	fmt.Fprintln(u.Out)
	if err != nil {
		return "", err
	}
	if len(value) == 0 {
		return def, nil
	}
	return string(value), nil
}

// WaitForKey waits for one of the named keys (say "return", "q", "escape")
// and returns it; without a terminal it returns "".
func (u *UI) WaitForKey(names ...string) (string, error) {
	if !u.Interactive() {
		return "", nil
	}
	pressed := ""
	err := u.withKeys(false, func(k key) bool {
		for _, n := range names {
			if k.name == n {
				pressed = n
				return true
			}
		}
		return false
	})
	return pressed, err
}
