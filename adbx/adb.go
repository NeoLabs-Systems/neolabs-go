package adbx

import (
	"context"
	"path/filepath"
	"runtime"
)

// BinPath returns the adb executable inside an Android SDK, or the bare name
// (resolved through PATH) when sdkDir is empty.
func BinPath(sdkDir string) string {
	name := "adb"
	if runtime.GOOS == "windows" {
		name = "adb.exe"
	}
	if sdkDir == "" {
		return name
	}
	return filepath.Join(sdkDir, "platform-tools", name)
}

// Client drives one device through the adb command line.
type Client struct {
	Bin    string
	Serial string
}

func (c Client) args(rest []string) []string {
	return append([]string{"-s", c.Serial}, rest...)
}

// Run executes `adb -s <serial> args...`.
func (c Client) Run(ctx context.Context, o RunOptions, args ...string) (Result, error) {
	return Run(ctx, c.Bin, c.args(args), o)
}

// Shell runs a command in the device shell and returns its standard output.
func (c Client) Shell(ctx context.Context, command string, o RunOptions) (string, error) {
	res, err := c.Run(ctx, o, "shell", command)
	return string(res.Stdout), err
}

// ExecOut runs `adb exec-out args...` and returns the raw bytes, which are
// binary safe (no CRLF translation), e.g. for `screencap -p`.
func (c Client) ExecOut(ctx context.Context, o RunOptions, args ...string) ([]byte, error) {
	res, err := c.Run(ctx, o, append([]string{"exec-out"}, args...)...)
	return res.Stdout, err
}

// PidAlive reports whether a process with this id exists.
func PidAlive(pid int) bool { return pid > 0 && pidAlive(pid) }
