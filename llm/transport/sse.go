package transport

import (
	"bufio"
	"io"
	"strings"
)

// SSEEvent is one dispatched server-sent event.
type SSEEvent struct {
	Event string
	Data  string
}

const maxSSELine = 64 << 20

// ReadSSE parses a text/event-stream body and calls fn for each event until the
// stream ends or fn returns an error.
func ReadSSE(r io.Reader, fn func(SSEEvent) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var event string
	var data []string
	dispatch := func() error {
		if event == "" && len(data) == 0 {
			return nil
		}
		ev := SSEEvent{Event: event, Data: strings.Join(data, "\n")}
		event, data = "", nil
		return fn(ev)
	}
	for {
		line, err := readLine(br)
		if err != nil {
			if err == io.EOF {
				return dispatch()
			}
			return err
		}
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
}

func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			if err == io.EOF && sb.Len() > 0 {
				return sb.String(), nil
			}
			return "", err
		}
		sb.Write(chunk)
		if !isPrefix {
			return sb.String(), nil
		}
		if sb.Len() > maxSSELine {
			return "", io.ErrShortBuffer
		}
	}
}
