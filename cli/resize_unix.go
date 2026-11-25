//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize delivers a signal on c when the terminal window changes size.
func notifyResize(c chan os.Signal) { signal.Notify(c, syscall.SIGWINCH) }
