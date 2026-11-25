package browserx

import (
	"os"
	"os/exec"
)

// StartDetached starts a program in its own session so it outlives the caller
// and is not killed with its process group. Its output is discarded. env
// replaces the environment when non-nil. The process is released, never
// waited on.
func StartDetached(executable string, args, env []string) error {
	cmd := exec.Command(executable, args...)
	cmd.Env = env
	cmd.SysProcAttr = detachedAttrs()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
