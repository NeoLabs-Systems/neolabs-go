package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Command is one subcommand of an app CLI.
type Command struct {
	Name    string
	Aliases []string
	Usage   string
	Summary string
	Hidden  bool
	Run     func(ctx context.Context, args []string) error
}

// Find returns the command named name (or aliased), or nil.
func Find(commands []Command, name string) *Command {
	for i := range commands {
		if commands[i].Name == name || slices.Contains(commands[i].Aliases, name) {
			return &commands[i]
		}
	}
	return nil
}

// Help renders the visible commands as an aligned list.
func Help(commands []Command) string {
	width := 0
	for _, c := range commands {
		if !c.Hidden && len(c.Usage) > width {
			width = len(c.Usage)
		}
	}
	var b strings.Builder
	for _, c := range commands {
		if c.Hidden {
			continue
		}
		fmt.Fprintf(&b, "  %-*s  %s\n", width, c.Usage, c.Summary)
	}
	return b.String()
}

// HasFlag reports whether args contains flag.
func HasFlag(args []string, flag string) bool { return slices.Contains(args, flag) }

// FlagValue returns the value of `--name value` or `--name=value`.
func FlagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				return args[i+1], true
			}
			return "", true
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// Positionals returns the arguments that are not flags. Flags listed in
// valued take the following argument as their value.
func Positionals(args []string, valued ...string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if slices.Contains(valued, a) {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}
