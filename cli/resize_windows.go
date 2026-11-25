package cli

import "os"

// notifyResize does nothing on Windows, which has no resize signal; blocks
// pick up the new width on their next redraw.
func notifyResize(chan os.Signal) {}
