package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/proc"
)

// HypervisorEntitlements is the entitlements plist that lets a qemu binary use
// Hypervisor.framework (hvf) on macOS.
const HypervisorEntitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>com.apple.security.hypervisor</key>
  <true/>
</dict>
</plist>
`

const codesignTimeout = 60 * time.Second

func codesign(ctx context.Context, args ...string) (string, error) {
	res, err := proc.Run(ctx, codesignTimeout, proc.Spec{Path: "codesign", Args: args}, 1<<20)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		if detail == "" {
			detail = "codesign failed"
		}
		return "", fmt.Errorf("%s", detail)
	}
	return res.Stdout + res.Stderr, nil
}

// HasHypervisorEntitlement reports whether binary is signed with the
// hypervisor entitlement. Only darwin hosts can have it.
func HasHypervisorEntitlement(ctx context.Context, binary string) bool {
	if runtime.GOOS != "darwin" || binary == "" {
		return false
	}
	res, err := proc.Run(ctx, codesignTimeout, proc.Spec{Path: "codesign", Args: []string{"--display", "--entitlements", "-", binary}}, 1<<20)
	if err != nil {
		return false
	}
	return strings.Contains(res.Stdout+res.Stderr, "com.apple.security.hypervisor")
}

// SignBinary ad-hoc signs a qemu binary, with the hypervisor entitlement when
// hypervisor is set.
func SignBinary(ctx context.Context, binary string, hypervisor bool) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if !hypervisor {
		_, err := codesign(ctx, "--force", "--sign", "-", binary)
		return err
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	entitlements := filepath.Join(os.TempDir(), fmt.Sprintf("neoagent-qemu-%d-%s.entitlements", os.Getpid(), hex.EncodeToString(suffix[:])))
	if err := os.WriteFile(entitlements, []byte(HypervisorEntitlements), 0o600); err != nil {
		return err
	}
	defer os.Remove(entitlements)
	_, err := codesign(ctx, "--force", "--sign", "-", "--entitlements", entitlements, binary)
	return err
}

// EnsureHypervisorSignature signs binary with the hypervisor entitlement
// unless it already has it, and reports whether the binary has it afterwards
// as a result of this call (false when it was already signed or signing
// failed, as in Node).
func EnsureHypervisorSignature(ctx context.Context, binary string) bool {
	if runtime.GOOS != "darwin" || binary == "" {
		return false
	}
	if HasHypervisorEntitlement(ctx, binary) {
		return false
	}
	if err := SignBinary(ctx, binary, true); err != nil {
		return false
	}
	return HasHypervisorEntitlement(ctx, binary)
}
