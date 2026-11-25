package cli

import "golang.org/x/term"

// keyMode switches the console to reading single keypresses. On Windows this
// changes input only, so output keeps its newline handling.
func keyMode(fd int) (func(), error) {
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(fd, old) }, nil
}
