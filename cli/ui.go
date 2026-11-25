package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ErrCancelled is returned when the user backs out of a prompt.
var ErrCancelled = Errorf("CANCELLED", "Cancelled.")

// UI prints human-readable output and asks questions. Silent turns all
// printing off (the `--json` mode, where stdout carries only events).
type UI struct {
	Out    io.Writer
	Err    io.Writer
	In     io.Reader
	Silent bool
	Color  bool
	reader *bufio.Reader
}

// NewUI writes to stdout/stderr and reads stdin, with color on a terminal.
func NewUI(silent bool) *UI {
	return &UI{Out: os.Stdout, Err: os.Stderr, In: os.Stdin, Silent: silent, Color: !silent && isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""}
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// Interactive reports whether questions can be asked.
func (u *UI) Interactive() bool {
	if u.Silent {
		return false
	}
	in, ok := u.In.(*os.File)
	out, ok2 := u.Out.(*os.File)
	return ok && ok2 && isTerminal(in) && isTerminal(out)
}

func (u *UI) paint(code, s string) string {
	if !u.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Bold, Muted, Accent and Command style text for the terminal.
func (u *UI) Bold(s string) string    { return u.paint("1", s) }
func (u *UI) Muted(s string) string   { return u.paint("2", s) }
func (u *UI) Accent(s string) string  { return u.paint("36", s) }
func (u *UI) Command(s string) string { return u.paint("36", "`"+s+"`") }

// Print writes a line unless silent.
func (u *UI) Print(format string, args ...any) {
	if !u.Silent {
		fmt.Fprintf(u.Out, format+"\n", args...)
	}
}

// Blank prints an empty line.
func (u *UI) Blank() { u.Print("") }

// Title prints a heading and an optional subtitle.
func (u *UI) Title(title, subtitle string) {
	u.Blank()
	u.Print("  %s", u.Bold(title))
	if subtitle != "" {
		u.Print("  %s", u.Muted(subtitle))
	}
}

// Section prints a section heading.
func (u *UI) Section(title string) {
	u.Blank()
	u.Print("  %s", u.Bold(title))
}

// Item prints a status line; tone is ok, warn, error or info.
func (u *UI) Item(tone, text, detail string) {
	mark := map[string]string{"ok": u.paint("32", "✓"), "warn": u.paint("33", "!"), "error": u.paint("31", "✕")}[tone]
	if mark == "" {
		mark = u.paint("36", "•")
	}
	if detail != "" {
		u.Print("  %s %s  %s", mark, text, u.Muted(detail))
		return
	}
	u.Print("  %s %s", mark, text)
}

// Note prints a hint line.
func (u *UI) Note(text string) { u.Print("  %s", u.Muted(text)) }

// Info, OK and Warn print one status line each. Warnings go to stderr even
// in silent mode so `--json` callers still see them in their logs.
func (u *UI) Info(text string) { u.Item("info", text, "") }
func (u *UI) OK(text string)   { u.Item("ok", text, "") }
func (u *UI) Warn(text string) {
	if u.Silent {
		fmt.Fprintln(u.Err, "warning: "+text)
		return
	}
	u.Item("warn", text, "")
}

func (u *UI) line() (string, error) {
	if u.reader == nil {
		u.reader = bufio.NewReader(u.In)
	}
	s, err := u.reader.ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", ErrCancelled
		}
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// Ask reads a line; an empty answer returns def.
func (u *UI) Ask(question, def string) (string, error) {
	prompt := "  " + question
	if def != "" {
		prompt += " " + u.Muted("("+def+")")
	}
	fmt.Fprint(u.Out, prompt+": ")
	v, err := u.line()
	if err != nil {
		return "", err
	}
	if v = strings.TrimSpace(v); v == "" {
		return def, nil
	}
	return v, nil
}

// AskSecret reads a line without echo; an empty answer keeps def.
func (u *UI) AskSecret(question, def string) (string, error) {
	hint := ""
	if def != "" {
		hint = " " + u.Muted("(enter keeps the saved value)")
	}
	fmt.Fprint(u.Out, "  "+question+hint+": ")
	in, ok := u.In.(*os.File)
	if !ok || !isTerminal(in) {
		v, err := u.line()
		if err != nil {
			return "", err
		}
		if v == "" {
			return def, nil
		}
		return v, nil
	}
	raw, err := term.ReadPassword(int(in.Fd()))
	fmt.Fprintln(u.Out)
	if err != nil {
		return "", err
	}
	if v := strings.TrimSpace(string(raw)); v != "" {
		return v, nil
	}
	return def, nil
}

// Confirm asks a yes/no question.
func (u *UI) Confirm(question string, def bool) (bool, error) {
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	v, err := u.Ask(question+" "+u.Muted("["+hint+"]"), "")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(v) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// Choice is one option of Select.
type Choice struct {
	Value string
	Label string
	Hint  string
}

// Select shows numbered choices and returns the chosen value.
func (u *UI) Select(question string, choices []Choice) (string, error) {
	u.Blank()
	u.Print("  %s", u.Bold(question))
	for i, c := range choices {
		line := fmt.Sprintf("    %d. %s", i+1, c.Label)
		if c.Hint != "" {
			line += "  " + u.Muted(c.Hint)
		}
		u.Print("%s", line)
	}
	for {
		v, err := u.Ask("Choose", "1")
		if err != nil {
			return "", err
		}
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(choices) {
			return choices[n-1].Value, nil
		}
		u.Print("  %s", u.Muted(fmt.Sprintf("Type a number from 1 to %d.", len(choices))))
	}
}
