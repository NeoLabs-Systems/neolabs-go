package realtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Engine.IO v4 packet types, the first character of a text packet.
const (
	eioOpen    = '0'
	eioClose   = '1'
	eioPing    = '2'
	eioPong    = '3'
	eioMessage = '4'
	eioUpgrade = '5'
	eioNoop    = '6'
)

// Socket.IO v5 packet types.
const (
	sioConnect      = 0
	sioDisconnect   = 1
	sioEvent        = 2
	sioAck          = 3
	sioConnectError = 4
	sioBinaryEvent  = 5
	sioBinaryAck    = 6
)

const (
	recordSeparator = '\x1e'
	maxAttachments  = 32
	maxAckIDDigits  = 15
)

// outPacket is one Engine.IO packet queued for a client.
type outPacket struct {
	kind  byte   // Engine.IO type
	text  string // payload of a text packet
	bin   []byte // payload of a binary message packet
	isBin bool
}

func (p outPacket) size() int { return len(p.text) + len(p.bin) + 1 }

// encodePollingPayload joins packets the way the polling transport sends them:
// text packets as type+data, binary packets as "b"+base64, separated by 0x1e.
func encodePollingPayload(pkts []outPacket) string {
	var b strings.Builder
	for i, p := range pkts {
		if i > 0 {
			b.WriteByte(recordSeparator)
		}
		if p.isBin {
			b.WriteByte('b')
			b.WriteString(base64.StdEncoding.EncodeToString(p.bin))
			continue
		}
		b.WriteByte(p.kind)
		b.WriteString(p.text)
	}
	return b.String()
}

// inPacket is one Engine.IO packet received from a client.
type inPacket struct {
	kind  byte
	text  string
	bin   []byte
	isBin bool
}

var errBadPacket = errors.New("realtime: malformed packet")

// decodePollingPayload splits a polling POST body into packets.
func decodePollingPayload(body string) ([]inPacket, error) {
	var out []inPacket
	for _, part := range strings.Split(body, string(recordSeparator)) {
		if part == "" {
			return nil, errBadPacket
		}
		if part[0] == 'b' {
			raw, err := base64.StdEncoding.DecodeString(part[1:])
			if err != nil {
				return nil, errBadPacket
			}
			out = append(out, inPacket{kind: eioMessage, bin: raw, isBin: true})
			continue
		}
		if part[0] < eioOpen || part[0] > eioNoop {
			return nil, errBadPacket
		}
		out = append(out, inPacket{kind: part[0], text: part[1:]})
	}
	return out, nil
}

// sioPacket is a decoded Socket.IO packet.
type sioPacket struct {
	typ         int
	nsp         string
	id          uint64
	hasID       bool
	data        string // raw JSON, empty when absent
	attachments int    // binary frames still to come
}

func (p sioPacket) isBinary() bool { return p.typ == sioBinaryEvent || p.typ == sioBinaryAck }

// decodeSIO parses the text of an Engine.IO message packet.
func decodeSIO(s string) (sioPacket, error) {
	var p sioPacket
	if s == "" || s[0] < '0' || s[0] > '6' {
		return p, errBadPacket
	}
	p.typ = int(s[0] - '0')
	p.nsp = "/"
	i := 1
	if p.isBinary() {
		end := strings.IndexByte(s[i:], '-')
		if end < 1 {
			return p, errBadPacket
		}
		n, err := strconv.Atoi(s[i : i+end])
		if err != nil || n < 0 || n > maxAttachments {
			return p, errBadPacket
		}
		p.attachments = n
		i += end + 1
	}
	if i < len(s) && s[i] == '/' {
		end := strings.IndexByte(s[i:], ',')
		if end < 0 {
			p.nsp, i = s[i:], len(s)
		} else {
			p.nsp, i = s[i:i+end], i+end+1
		}
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > start {
		if i-start > maxAckIDDigits {
			return p, errBadPacket
		}
		id, err := strconv.ParseUint(s[start:i], 10, 64)
		if err != nil {
			return p, errBadPacket
		}
		p.id, p.hasID = id, true
	}
	p.data = s[i:]
	return p, validateSIOData(p)
}

func validateSIOData(p sioPacket) error {
	switch p.typ {
	case sioConnect, sioConnectError:
		if p.data == "" {
			return nil
		}
		var v map[string]any
		if json.Unmarshal([]byte(p.data), &v) != nil {
			return errBadPacket
		}
	case sioDisconnect:
		if p.data != "" {
			return errBadPacket
		}
	case sioEvent, sioBinaryEvent:
		var v []any
		if json.Unmarshal([]byte(p.data), &v) != nil || len(v) == 0 {
			return errBadPacket
		}
		if _, ok := v[0].(string); !ok {
			return errBadPacket
		}
	case sioAck, sioBinaryAck:
		var v []any
		if json.Unmarshal([]byte(p.data), &v) != nil {
			return errBadPacket
		}
	}
	return nil
}

// args decodes an event or ack payload and swaps binary placeholders for the
// attachments that arrived with it. For an event the first element is the name.
func (p sioPacket) args(attachments [][]byte) ([]any, error) {
	dec := json.NewDecoder(strings.NewReader(p.data))
	var v []any
	if err := dec.Decode(&v); err != nil {
		return nil, errBadPacket
	}
	for i := range v {
		restored, err := restoreBinary(v[i], attachments)
		if err != nil {
			return nil, err
		}
		v[i] = restored
	}
	return v, nil
}

func restoreBinary(v any, attachments [][]byte) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if ph, _ := t["_placeholder"].(bool); ph {
			num, ok := t["num"].(float64)
			if !ok || num < 0 || int(num) >= len(attachments) || num != float64(int(num)) {
				return nil, errBadPacket
			}
			return attachments[int(num)], nil
		}
		for k, item := range t {
			restored, err := restoreBinary(item, attachments)
			if err != nil {
				return nil, err
			}
			t[k] = restored
		}
	case []any:
		for i, item := range t {
			restored, err := restoreBinary(item, attachments)
			if err != nil {
				return nil, err
			}
			t[i] = restored
		}
	}
	return v, nil
}

// marshalJSON encodes like JSON.stringify: no HTML escaping, no trailing newline.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// extractBinary replaces []byte values (at the top level and inside maps and
// slices) by Socket.IO placeholders and returns the attachments in order.
func extractBinary(args []any) ([]any, [][]byte) {
	var bins [][]byte
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case []byte:
			bins = append(bins, t)
			return map[string]any{"_placeholder": true, "num": len(bins) - 1}
		case map[string]any:
			out := make(map[string]any, len(t))
			for _, k := range slices.Sorted(maps.Keys(t)) { // the order JSON is written in
				out[k] = walk(t[k])
			}
			return out
		case []any:
			out := make([]any, len(t))
			for i, item := range t {
				out[i] = walk(item)
			}
			return out
		}
		return v
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = walk(a)
	}
	return out, bins
}

// encodeSIO builds the text head of a packet. data may be nil.
func encodeSIO(typ int, nsp string, id *uint64, attachments int, data []byte) string {
	var b strings.Builder
	b.WriteByte(byte('0' + typ))
	if typ == sioBinaryEvent || typ == sioBinaryAck {
		b.WriteString(strconv.Itoa(attachments))
		b.WriteByte('-')
	}
	if nsp != "" && nsp != "/" {
		b.WriteString(nsp)
		b.WriteByte(',')
	}
	if id != nil {
		b.WriteString(strconv.FormatUint(*id, 10))
	}
	b.Write(data)
	return b.String()
}

// encodeMessage renders an EVENT (name set) or ACK (name empty) with its
// binary attachments as the Engine.IO packets to queue, in order.
func encodeMessage(nsp, event string, id *uint64, args []any) ([]outPacket, error) {
	isEvent := event != ""
	payload := args
	if isEvent {
		payload = append([]any{event}, args...)
	}
	payload, bins := extractBinary(payload)
	if payload == nil {
		payload = []any{}
	}
	data, err := marshalJSON(payload)
	if err != nil {
		return nil, err
	}
	typ := sioEvent
	if !isEvent {
		typ = sioAck
	}
	if len(bins) > 0 {
		typ += 3 // BINARY_EVENT / BINARY_ACK
	}
	pkts := []outPacket{{kind: eioMessage, text: encodeSIO(typ, nsp, id, len(bins), data)}}
	for _, b := range bins {
		pkts = append(pkts, outPacket{kind: eioMessage, bin: b, isBin: true})
	}
	return pkts, nil
}
