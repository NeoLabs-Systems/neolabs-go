package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// Terminal output for an app CLI: the brand palette, a handful of line shapes,
// and a quiet mode for --json runs where stdout belongs to events. Every line
// fits the window: anything wider is cut, never wrapped, so the live blocks
// that redraw in place always know how many lines they own.

// ErrCancelled is returned when the user backs out of a prompt.
var ErrCancelled = Errorf("CLI_CANCELLED", "Cancelled.")

// Color is one palette entry at each terminal color depth.
type Color struct {
	RGB     [3]int
	ANSI256 int
	ANSI16  int
}

// Palette is the NeoLabs dark-surface palette (the apps' --app-* tokens) and
// the mascot's colors: a green-grey rim around a near-black screen of dim dots.
var Palette = map[string]Color{
	"accent": {[3]int{225, 176, 82}, 179, 33},
	"ok":     {[3]int{116, 192, 124}, 114, 32},
	"error":  {[3]int{222, 138, 120}, 174, 31},
	"muted":  {[3]int{174, 183, 166}, 145, 37},
	"faint":  {[3]int{126, 136, 119}, 102, 90},
	"rim":    {[3]int{68, 80, 73}, 239, 90},
	"screen": {[3]int{5, 8, 6}, 232, 40},
	"unlit":  {[3]int{64, 72, 68}, 238, 90},
}

// Tone is how a status line is marked: ok, warn, error, pending or info.
type Tone struct{ Mark, Color string }

// Tones are the status marks.
var Tones = map[string]Tone{
	"ok":      {"✓", "ok"},
	"warn":    {"!", "accent"},
	"error":   {"✕", "error"},
	"pending": {"○", "faint"},
	"info":    {"•", "faint"},
}

// liveBlock is a block of lines (a step list) redrawn in place at the bottom
// of the screen. printAbove writes lines above it and draws it again below.
type liveBlock interface {
	printAbove(write func())
}

// UI prints human-readable output and asks questions. Silent turns all
// printing off (the `--json` mode, where stdout carries only events).
type UI struct {
	Out    io.Writer
	Err    io.Writer
	In     io.Reader
	Silent bool
	// Depth is the color depth: 0 (none), 4, 8 or 24 bits.
	Depth int

	live  liveBlock
	steps *StepList
	lines *lineReader
}

// NewUI writes to stdout/stderr and reads stdin, with the color depth the
// terminal supports.
func NewUI(silent bool) *UI {
	u := &UI{Out: os.Stdout, Err: os.Stderr, In: os.Stdin, Silent: silent}
	if !silent {
		u.Depth = detectColorDepth(os.Stdout, os.Getenv)
	}
	return u
}

func detectColorDepth(f *os.File, getenv func(string) string) int {
	_, noColor := os.LookupEnv("NO_COLOR")
	if !isTerminal(f) || noColor || getenv("TERM") == "dumb" {
		return 0
	}
	if ct := strings.ToLower(getenv("COLORTERM")); strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit") {
		return 24
	}
	if strings.Contains(getenv("TERM"), "256") {
		return 8
	}
	return 4
}

func isTerminal(w any) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Interactive reports whether questions can be asked.
func (u *UI) Interactive() bool { return !u.Silent && isTerminal(u.In) && isTerminal(u.Out) }

// CanRedraw reports whether blocks can be redrawn in place.
func (u *UI) CanRedraw() bool { return !u.Silent && isTerminal(u.Out) }

// Width is the window width (80 without a terminal, at least 20).
func (u *UI) Width() int {
	if f, ok := u.Out.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			return max(20, w)
		}
	}
	return 80
}

// IsNarrow reports a window too narrow for side-by-side layouts.
func (u *UI) IsNarrow() bool { return u.Width() < 64 }

// --- Colors ------------------------------------------------------------------

// SGR wraps text in an SGR escape (nothing without colors).
func (u *UI) SGR(code, text string) string {
	if u.Depth == 0 {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// Foreground is the SGR code of a palette color as text color.
func (u *UI) Foreground(name string) string {
	c := Palette[name]
	switch u.Depth {
	case 24:
		return fmt.Sprintf("38;2;%d;%d;%d", c.RGB[0], c.RGB[1], c.RGB[2])
	case 8:
		return "38;5;" + strconv.Itoa(c.ANSI256)
	}
	return strconv.Itoa(c.ANSI16)
}

// Background is the SGR code of a palette color as background.
func (u *UI) Background(name string) string {
	c := Palette[name]
	switch u.Depth {
	case 24:
		return fmt.Sprintf("48;2;%d;%d;%d", c.RGB[0], c.RGB[1], c.RGB[2])
	case 8:
		return "48;5;" + strconv.Itoa(c.ANSI256)
	}
	return strconv.Itoa(c.ANSI16 + 10)
}

// Accent, Error, Muted, Faint, Bold, Command and InTone style text.
func (u *UI) Accent(s string) string  { return u.SGR(u.Foreground("accent"), s) }
func (u *UI) Error(s string) string   { return u.SGR(u.Foreground("error"), s) }
func (u *UI) Muted(s string) string   { return u.SGR(u.Foreground("muted"), s) }
func (u *UI) Faint(s string) string   { return u.SGR(u.Foreground("faint"), s) }
func (u *UI) Bold(s string) string    { return u.SGR("1", s) }
func (u *UI) Command(s string) string { return u.SGR("1;"+u.Foreground("accent"), s) }
func (u *UI) InTone(tone, s string) string {
	return u.SGR(u.Foreground(Tones[tone].Color), s)
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// VisibleLength is the number of characters text shows.
func VisibleLength(text string) int {
	return utf8.RuneCountInString(ansiPattern.ReplaceAllString(text, ""))
}

// PadEnd pads text with spaces to width visible characters.
func PadEnd(text string, width int) string {
	return text + strings.Repeat(" ", max(0, width-VisibleLength(text)))
}

// Truncate cuts a styled line to max visible characters, ending it with "…".
func (u *UI) Truncate(text string, limit int) string {
	if VisibleLength(text) <= limit {
		return text
	}
	var b strings.Builder
	shown := 0
	for len(text) > 0 {
		if loc := ansiPattern.FindStringIndex(text); loc != nil && loc[0] == 0 {
			b.WriteString(text[:loc[1]])
			text = text[loc[1]:]
			continue
		}
		r, size := utf8.DecodeRuneInString(text)
		if shown < limit-1 {
			b.WriteRune(r)
			shown++
		}
		text = text[size:]
	}
	if u.Depth == 0 {
		return b.String() + "…"
	}
	return b.String() + "…\x1b[0m"
}

// Wrap breaks plain text into lines of at most limit characters, on spaces.
func Wrap(text string, limit int) []string {
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		line := ""
		for _, word := range strings.Fields(paragraph) {
			if line != "" && VisibleLength(line+" "+word) > limit {
				lines = append(lines, line)
				line = word
			} else if line == "" {
				line = word
			} else {
				line += " " + word
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// --- Lines -------------------------------------------------------------------

func (u *UI) fit(line string) string {
	if isTerminal(u.Out) {
		return u.Truncate(line, u.Width()-1)
	}
	return line
}

// Print writes a line unless silent. While a live block shows, the line goes
// above it and the block is drawn again below.
func (u *UI) Print(line string) {
	if u.Silent {
		return
	}
	if u.live != nil {
		u.live.printAbove(func() { fmt.Fprintln(u.Out, u.fit(line)) })
		return
	}
	fmt.Fprintln(u.Out, u.fit(line))
}

// Printf prints a formatted line.
func (u *UI) Printf(format string, args ...any) { u.Print(fmt.Sprintf(format, args...)) }

// drawBlock draws lines that will be redrawn in place and returns their
// widths, which clearBlock needs: a narrower window re-wraps drawn lines.
func (u *UI) drawBlock(lines []string) []int {
	widths := make([]int, len(lines))
	shown := make([]string, len(lines))
	for i, l := range lines {
		shown[i] = u.fit(l)
		widths[i] = VisibleLength(shown[i])
	}
	fmt.Fprint(u.Out, strings.Join(shown, "\n")+"\n")
	return widths
}

func (u *UI) clearBlock(widths []int) {
	cols := u.Width()
	rows := 0
	for _, w := range widths {
		rows += max(1, (w+cols-1)/cols)
	}
	if rows > 0 {
		fmt.Fprintf(u.Out, "\x1b[%dF\x1b[J", rows)
	}
}

// Blank prints an empty line.
func (u *UI) Blank() { u.Print("") }

// Title prints a heading and an optional subtitle.
func (u *UI) Title(title, subtitle string) {
	u.Blank()
	u.Print("  " + u.Bold(title))
	if subtitle != "" {
		u.Paragraph(subtitle, 2, u.Muted)
	}
}

// Section prints a section heading.
func (u *UI) Section(title string) {
	u.Blank()
	u.Print("  " + u.Bold(title))
}

// ItemLine is a status line: a tone mark, the label (padded to labelWidth
// when it is not 0) and a muted detail.
func (u *UI) ItemLine(tone, label, detail string, labelWidth int) string {
	if labelWidth > 0 {
		label = PadEnd(label, labelWidth)
	}
	suffix := ""
	if detail != "" {
		suffix = "  " + u.Muted(detail)
	}
	return strings.TrimRight("  "+u.InTone(tone, Tones[tone].Mark)+" "+label+suffix, " ")
}

// Item prints a status line; tone is ok, warn, error, pending or info.
func (u *UI) Item(tone, label, detail string) { u.Print(u.ItemLine(tone, label, detail, 0)) }

// ItemWidth prints a status line with the label padded to labelWidth.
func (u *UI) ItemWidth(tone, label, detail string, labelWidth int) {
	u.Print(u.ItemLine(tone, label, detail, labelWidth))
}

// Paragraph prints prose that wraps to the window instead of being cut.
func (u *UI) Paragraph(text string, indent int, style func(string) string) {
	if style == nil {
		style = func(s string) string { return s }
	}
	for _, line := range Wrap(text, u.Width()-indent-1) {
		u.Print(strings.Repeat(" ", indent) + style(line))
	}
}

// Note prints a muted paragraph.
func (u *UI) Note(text string) { u.Paragraph(text, 2, u.Muted) }

// Keys prints a footer of keys and what they do.
func (u *UI) Keys(pairs ...[2]string) {
	shown := make([]string, len(pairs))
	for i, p := range pairs {
		shown[i] = p[0] + " " + u.Faint(p[1])
	}
	u.Print("  " + strings.Join(shown, u.Faint("  ·  ")))
}

// Columns prints two columns side by side (say, a mascot and a greeting), or
// stacked when the window is too narrow for both.
func (u *UI) Columns(left, right []string, gap int) {
	lw, rw := 0, 0
	for _, l := range left {
		lw = max(lw, VisibleLength(l))
	}
	for _, r := range right {
		rw = max(rw, VisibleLength(r))
	}
	if 2+lw+gap+rw >= u.Width() {
		if 2+lw < u.Width() {
			for _, l := range left {
				u.Print("  " + l)
			}
			u.Blank()
		}
		for _, r := range right {
			u.Print("  " + r)
		}
		return
	}
	offset := max(0, (len(left)-len(right))/2)
	for i, l := range left {
		r := ""
		if j := i - offset; j >= 0 && j < len(right) {
			r = right[j]
		}
		u.Print(strings.TrimRight("  "+PadEnd(l, lw)+strings.Repeat(" ", gap)+r, " "))
	}
}

// Info and OK report progress: while a step list shows, they become the
// running step's detail instead of scrolling past it. Warn always prints; in
// silent mode it goes to stderr so `--json` callers still see it in their logs.
func (u *UI) Info(text string) { u.report("info", text) }
func (u *UI) OK(text string)   { u.report("ok", text) }
func (u *UI) Warn(text string) { u.report("warn", text) }

func (u *UI) report(tone, text string) {
	if u.Silent {
		if tone == "warn" {
			fmt.Fprintln(u.Err, "warning: "+text)
		}
		return
	}
	if tone != "warn" && u.steps != nil && u.steps.Note(text) {
		return
	}
	u.Item(tone, text, "")
}
