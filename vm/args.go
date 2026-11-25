// Package vm holds the generic building blocks for running QEMU guests:
// command-line assembly helpers, QMP, qemu-img, accelerator selection, local
// socket handling, orphan reclaiming, and macOS hypervisor signing. What a
// particular product boots (devices, disks, images) stays with the product.
package vm

import "strings"

// Args assembles a qemu command line. Methods append in call order, so the
// caller owns the order exactly as a hand-written argument list would.
type Args struct{ list []string }

// Add appends a flag and its value.
func (a *Args) Add(flag, value string) *Args {
	a.list = append(a.list, flag, value)
	return a
}

// Flag appends values as they are (a bare flag, or several flag/value pairs).
func (a *Args) Flag(values ...string) *Args {
	a.list = append(a.list, values...)
	return a
}

// List returns the assembled arguments.
func (a *Args) List() []string { return a.list }

// EscapeValue doubles commas, which qemu reads as option separators, so a file
// name can sit inside a -drive or -device option string.
func EscapeValue(value string) string { return strings.ReplaceAll(value, ",", ",,") }

// Option builds "name,key=value,..." from a name and key/value pairs. A pair
// with an empty value is written as a bare key, as in "romfile=" is not; use a
// literal "key=" value for that.
func Option(name string, parts ...string) string {
	return strings.Join(append([]string{name}, parts...), ",")
}
