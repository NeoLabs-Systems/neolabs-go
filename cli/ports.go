package cli

import (
	"context"
	"net"
	"strconv"
	"time"
)

// PortOpen reports whether something accepts TCP connections on
// 127.0.0.1:port.
func PortOpen(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 700*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// A server listening without a host binds dual-stack; macOS still lets that
// bind succeed while another program holds 0.0.0.0 or 127.0.0.1, so those are
// probed as well.
var portProbeHosts = []string{"", "0.0.0.0", "127.0.0.1"}

// PortAvailable reports whether a server could listen on port.
func PortAvailable(port int) bool {
	for _, host := range portProbeHosts {
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		l.Close()
	}
	return true
}

// FindAvailablePort returns the first free port from preferred on, trying 100.
func FindAvailablePort(preferred, fallback int) (int, error) {
	start := preferred
	if start < 1 || start > 65535 {
		start = fallback
	}
	for p := start; p < start+100 && p <= 65535; p++ {
		if PortAvailable(p) {
			return p, nil
		}
	}
	return 0, Errorf("SETUP_PORT_UNAVAILABLE", "No available port was found from %d.", start)
}
