package webextract

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// value is parsed JSON that keeps object key order, so it is written back the
// way JSON.stringify writes what JSON.parse read.
type value struct {
	kind  byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'z' null
	keys  []string
	vals  []*value
	str   string
	num   float64
	truth bool
}

func parseJSON(raw string) (*value, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (*value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &value{kind: 'o'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key := kt.(string)
				child, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				v.set(key, child)
			}
			_, err := dec.Token()
			return v, err
		case '[':
			v := &value{kind: 'a'}
			for dec.More() {
				child, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				v.vals = append(v.vals, child)
			}
			_, err := dec.Token()
			return v, err
		}
	case string:
		return &value{kind: 's', str: t}, nil
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, err
		}
		return &value{kind: 'n', num: f}, nil
	case bool:
		return &value{kind: 'b', truth: t}, nil
	case nil:
		return &value{kind: 'z'}, nil
	}
	return nil, errors.New("unexpected token")
}

// set assigns a key; like a JavaScript object a repeated key keeps its first
// position and takes the last value.
func (v *value) set(key string, child *value) {
	for i, k := range v.keys {
		if k == key {
			v.vals[i] = child
			return
		}
	}
	v.keys = append(v.keys, key)
	v.vals = append(v.vals, child)
}

func (v *value) get(key string) *value {
	if v == nil || v.kind != 'o' {
		return nil
	}
	for i, k := range v.keys {
		if k == key {
			return v.vals[i]
		}
	}
	return nil
}

// truthy is JavaScript truthiness.
func (v *value) truthy() bool {
	if v == nil {
		return false
	}
	switch v.kind {
	case 'z':
		return false
	case 'b':
		return v.truth
	case 'n':
		return v.num != 0 && !math.IsNaN(v.num)
	case 's':
		return v.str != ""
	}
	return true
}

// stringify is JSON.stringify(v, null, indent) for indent 0 or 2.
func (v *value) stringify(indent int) string {
	var b bytes.Buffer
	v.write(&b, indent, 0)
	return b.String()
}

func (v *value) write(b *bytes.Buffer, indent, depth int) {
	newline := func(d int) {
		if indent > 0 {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(" ", indent*d))
		}
	}
	switch v.kind {
	case 'z':
		b.WriteString("null")
	case 'b':
		b.WriteString(strconv.FormatBool(v.truth))
	case 'n':
		b.WriteString(jsNumber(v.num))
	case 's':
		writeJSString(b, v.str)
	case 'a':
		if len(v.vals) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, c := range v.vals {
			if i > 0 {
				b.WriteByte(',')
			}
			newline(depth + 1)
			c.write(b, indent, depth+1)
		}
		newline(depth)
		b.WriteByte(']')
	case 'o':
		if len(v.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, k := range v.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			newline(depth + 1)
			writeJSString(b, k)
			b.WriteByte(':')
			if indent > 0 {
				b.WriteByte(' ')
			}
			v.vals[i].write(b, indent, depth+1)
		}
		newline(depth)
		b.WriteByte('}')
	}
}

// jsNumber formats a number like JavaScript's Number#toString.
func jsNumber(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	abs := math.Abs(f)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exp, _ := strings.Cut(s, "e")
	sign := exp[0]
	exp = strings.TrimLeft(exp[1:], "0")
	return mantissa + "e" + string(sign) + exp
}

// writeJSString quotes like JSON.stringify: only quotes, backslashes and
// control characters are escaped.
func writeJSString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteString(strconv.FormatInt(int64(r)>>4, 16))
			b.WriteString(strconv.FormatInt(int64(r)&0xf, 16))
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}
