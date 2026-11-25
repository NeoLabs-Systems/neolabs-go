//go:build windows

package vm

import "os"

func ownedPrivately(os.FileInfo) bool { return true }
