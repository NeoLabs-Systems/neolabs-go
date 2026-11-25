// Package transport is the HTTP plumbing shared by the provider clients:
// insertion-ordered JSON objects (so request bodies match what the Node
// providers send byte for byte), a client that refuses internal addresses unless
// told otherwise, bounded response reading and a server-sent-events reader.
package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

// Obj is a JSON object that keeps insertion order, like a JavaScript object.
type Obj struct {
	keys []string
	vals map[string]any
}

// NewObj returns an empty object.
func NewObj() *Obj { return &Obj{vals: map[string]any{}} }

// FromMap copies a map into an object with its keys sorted.
func FromMap(m map[string]any) *Obj {
	o := NewObj()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, m[k])
	}
	return o
}

// Set assigns a key; an existing key keeps its position.
func (o *Obj) Set(key string, value any) *Obj {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
	return o
}

// Get reads a key.
func (o *Obj) Get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Has reports whether the key is set.
func (o *Obj) Has(key string) bool { _, ok := o.vals[key]; return ok }

// Delete removes a key.
func (o *Obj) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

// Clone copies the top level of the object.
func (o *Obj) Clone() *Obj {
	c := NewObj()
	for _, k := range o.keys {
		c.Set(k, o.vals[k])
	}
	return c
}

// Keys returns the keys in order.
func (o *Obj) Keys() []string { return append([]string(nil), o.keys...) }

// MarshalJSON writes the keys in insertion order.
func (o *Obj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Marshal is JSON.stringify: no HTML escaping and no trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ParseOrdered decodes JSON keeping object key order (objects become *Obj,
// arrays []any, numbers float64), so it can be written back the way JavaScript's
// JSON.parse + JSON.stringify would write it.
func ParseOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	v, err := decodeOrdered(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: trailing data")
	}
	return v, nil
}

func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := NewObj()
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				o.Set(keyTok.(string), val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			list := []any{}
			for dec.More() {
				val, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
	}
	return tok, nil
}

// String returns the key's value if it is a string.
func (o *Obj) String(key string) string {
	s, _ := o.vals[key].(string)
	return s
}

// Object returns the key's value if it is an object.
func (o *Obj) Object(key string) *Obj {
	v, _ := o.vals[key].(*Obj)
	return v
}

// List returns the key's value if it is an array.
func (o *Obj) List(key string) []any {
	v, _ := o.vals[key].([]any)
	return v
}

// FromMapTypeFirst is FromMap with the "type" key first, the shape APIs return
// typed items in, for items that went through a map and lost their order.
func FromMapTypeFirst(m map[string]any) *Obj {
	o := NewObj()
	if t, ok := m["type"]; ok {
		o.Set("type", t)
	}
	for _, k := range FromMap(m).Keys() {
		if k != "type" {
			o.Set(k, m[k])
		}
	}
	return o
}
