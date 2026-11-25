package vm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// MaxSocketPathBytes is the sockaddr_un limit that applies on macOS and is the
// stricter of the platforms qemu runs on.
const MaxSocketPathBytes = 104

// Address is where a local qemu service (VNC, QMP) listens: a UNIX socket
// path, or on Windows a loopback TCP port.
type Address struct {
	Path string
	Port int
}

// Dial connects to the address.
func (a Address) Dial(timeout time.Duration) (net.Conn, error) {
	if a.Path != "" {
		return net.DialTimeout("unix", a.Path, timeout)
	}
	return net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(a.Port)), timeout)
}

// String is the address as it appears in logs.
func (a Address) String() string {
	if a.Path != "" {
		return "unix:" + a.Path
	}
	return fmt.Sprintf("127.0.0.1:%d", a.Port)
}

// IsZero reports an unset address.
func (a Address) IsZero() bool { return a.Path == "" && a.Port == 0 }

// FreePort returns an unused loopback port.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// FreeVNCPort finds a free 5900+display port (display 10 to 99) on loopback,
// the form qemu's -vnc option takes on hosts without UNIX sockets.
func FreeVNCPort() (int, error) {
	for display := 10; display < 100; display++ {
		port := 5900 + display
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		l.Close()
		return port, nil
	}
	return 0, errors.New("No loopback VNC display is available.")
}

// InstanceSocket picks where a qemu service for an instance listens. The guest
// reaches the host's loopback through 10.0.2.2 and qemu's VNC and QMP have no
// authentication, so on POSIX hosts they are always UNIX sockets no guest can
// connect to. A path deeper than the sockaddr_un limit moves to a short private
// directory under shortRoot. Windows has no such sockets and gets fallbackPort.
func InstanceSocket(instanceDir, name string, fallbackPort func() (int, error), shortRoot string, windows bool) (Address, error) {
	if windows {
		port, err := fallbackPort()
		return Address{Port: port}, err
	}
	socket := filepath.Join(instanceDir, name)
	if len(socket) >= MaxSocketPathBytes {
		dir, err := PrivateShortDir(instanceDir, shortRoot)
		if err != nil {
			return Address{}, err
		}
		socket = filepath.Join(dir, name)
	}
	if len(socket) >= MaxSocketPathBytes {
		return Address{}, fmt.Errorf("The computer's socket path %s is too long.", socket)
	}
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return Address{}, err
	}
	return Address{Path: socket}, nil
}

// PrivateShortDir returns a per-instance directory under shortRoot. A fixed
// path in a shared directory is only used once it is shown to be this
// process's own private directory and not something another account created.
func PrivateShortDir(instanceDir, shortRoot string) (string, error) {
	root := filepath.Join(shortRoot, fmt.Sprintf("neoagent-%d", os.Getuid()))
	if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || !ownedPrivately(info) {
		return "", fmt.Errorf("%s is not a private directory owned by this account; remove it and start the computer again.", root)
	}
	dir := filepath.Join(root, filepath.Base(instanceDir))
	return dir, os.MkdirAll(dir, 0o700)
}

// WaitForSocket polls until the address accepts a connection, the deadline
// passes, or isDead reports the owning process gone. QEMU opens its VNC server
// a moment after the process exists, and handing out a display session before
// then would refuse the first viewer.
func WaitForSocket(addr Address, timeout time.Duration, isDead func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isDead != nil && isDead() {
			return false
		}
		if conn, err := addr.Dial(time.Second); err == nil {
			conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
