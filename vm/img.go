package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/proc"
)

// Img drives qemu-img.
type Img struct{ Binary string }

const imgTimeout = 120 * time.Second

func (i Img) run(ctx context.Context, args ...string) (string, error) {
	out, err := proc.Checked(ctx, imgTimeout, proc.Spec{Path: i.Binary, Args: args})
	if err != nil {
		first := ""
		if len(args) > 0 {
			first = args[0]
		}
		return "", fmt.Errorf("%s %s failed: %s", filepath.Base(i.Binary), first, strings.TrimPrefix(err.Error(), i.Binary+": "))
	}
	return out, nil
}

// CreateOverlay creates a qcow2 image backed by base (format backingFormat).
func (i Img) CreateOverlay(ctx context.Context, path, base, backingFormat string) error {
	_, err := i.run(ctx, "create", "-f", "qcow2", "-F", backingFormat, "-b", base, path)
	return err
}

// Create makes an empty qcow2 image of the given size ("12G").
func (i Img) Create(ctx context.Context, path, size string) error {
	_, err := i.run(ctx, "create", "-f", "qcow2", path, size)
	return err
}

// DiskInfo is the part of `qemu-img info` the runtime reads.
type DiskInfo struct {
	VirtualSize int64 `json:"virtual-size"`
	ActualSize  int64 `json:"actual-size"`
}

// Info reads an image. `-U` reads one a running guest still holds open;
// without it every query against a live computer fails on the write lock. A
// failure yields nil.
func (i Img) Info(ctx context.Context, path string) *DiskInfo {
	out, err := i.run(ctx, "info", "--output=json", "-U", path)
	if err != nil {
		return nil
	}
	var info DiskInfo
	if json.Unmarshal([]byte(out), &info) != nil {
		return nil
	}
	return &info
}

// EnsureSize grows an image to at least minimumGiB. An image whose size could
// not be read is left alone: resizing could mean writing to one that is still
// open elsewhere, and qemu reports the real problem at start.
func (i Img) EnsureSize(ctx context.Context, path string, minimumGiB int64) error {
	info := i.Info(ctx, path)
	if info == nil || info.VirtualSize <= 0 || info.VirtualSize >= minimumGiB<<30 {
		return nil
	}
	_, err := i.run(ctx, "resize", path, fmt.Sprintf("%dG", minimumGiB))
	return err
}

// WalkDisks lists the .qcow2 files under directory.
func WalkDisks(directory string) []string {
	var files []string
	_ = filepath.WalkDir(directory, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".qcow2") {
			files = append(files, path)
		}
		return nil
	})
	return files
}

// SparseLiability sums how far the disks under directories may still grow
// (virtual size minus actual size). Frozen disks, such as a retired system
// disk, cannot grow and do not count.
func (i Img) SparseLiability(ctx context.Context, directories []string, frozen func(path string) bool) int64 {
	seen := map[string]bool{}
	var total int64
	for _, dir := range directories {
		for _, disk := range WalkDisks(dir) {
			if seen[disk] || (frozen != nil && frozen(disk)) {
				continue
			}
			seen[disk] = true
			info := i.Info(ctx, disk)
			if info == nil {
				continue
			}
			if grow := info.VirtualSize - info.ActualSize; grow > 0 {
				total += grow
			}
		}
	}
	return total
}

// DiskCapacity reports the free and total bytes of the file system holding
// directory, or zeros when it cannot be read.
func DiskCapacity(directory string) (availableBytes, totalBytes int64) {
	if _, err := os.Stat(directory); err != nil {
		return 0, 0
	}
	return diskCapacity(directory)
}
