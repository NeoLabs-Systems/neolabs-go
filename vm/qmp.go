package vm

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// QMPCommand connects to a QMP socket, negotiates capabilities, runs one
// command and returns its "return" value. A QMP error or a connection that
// closes early is an error.
func QMPCommand(ctx context.Context, addr Address, execute string, arguments any, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type outcome struct {
		value json.RawMessage
		err   error
	}
	result := make(chan outcome, 1)
	conn, err := addr.Dial(timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	go func() {
		v, err := qmpExchange(conn, execute, arguments)
		result <- outcome{v, err}
	}()
	select {
	case r := <-result:
		return r.value, r.err
	case <-ctx.Done():
		conn.Close()
		return nil, fmt.Errorf("QMP %s timed out.", execute)
	}
}

func qmpExchange(conn interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
}, execute string, arguments any) (json.RawMessage, error) {
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, err
	}
	requestID := hex.EncodeToString(idBytes[:])
	reader := bufio.NewReader(conn)
	capabilitiesReady := false
	for {
		line, err := reader.ReadString('\n')
		if strings.TrimSpace(line) != "" {
			var msg struct {
				QMP    json.RawMessage `json:"QMP"`
				ID     string          `json:"id"`
				Return json.RawMessage `json:"return"`
				Error  *struct {
					Desc string `json:"desc"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(line), &msg) == nil {
				switch {
				case msg.QMP != nil && !capabilitiesReady:
					if _, err := conn.Write([]byte(`{"execute":"qmp_capabilities","id":"capabilities"}` + "\r\n")); err != nil {
						return nil, err
					}
				case msg.ID == "capabilities" && msg.Error == nil:
					capabilitiesReady = true
					request := map[string]any{"execute": execute, "id": requestID}
					if arguments != nil {
						request["arguments"] = arguments
					}
					body, _ := json.Marshal(request)
					if _, err := conn.Write(append(body, '\r', '\n')); err != nil {
						return nil, err
					}
				case msg.ID == requestID:
					if msg.Error != nil {
						desc := msg.Error.Desc
						if desc == "" {
							desc = fmt.Sprintf("QMP %s failed.", execute)
						}
						return nil, errors.New(desc)
					}
					return msg.Return, nil
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("QMP %s connection closed unexpectedly.", execute)
		}
	}
}

// Powerdown asks the guest to shut down through ACPI. Failure is ignored: the
// caller escalates to signals when the guest does not stop.
func Powerdown(ctx context.Context, addr Address) {
	_, _ = QMPCommand(ctx, addr, "system_powerdown", nil, 2500*time.Millisecond)
}
