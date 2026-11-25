package realtime

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The vectors come from the socket.io-parser of the Node server (gen/vectors.js).
type vectorFile struct {
	Events []struct {
		NSP     string            `json:"nsp"`
		Event   string            `json:"event"`
		Args    []any             `json:"args"`
		ID      *uint64           `json:"id"`
		Encoded []json.RawMessage `json:"encoded"`
	} `json:"events"`
	Acks []struct {
		NSP     string            `json:"nsp"`
		ID      uint64            `json:"id"`
		Args    []any             `json:"args"`
		Encoded []json.RawMessage `json:"encoded"`
	} `json:"acks"`
	Decode []struct {
		Encoded []json.RawMessage `json:"encoded"`
		Type    int               `json:"type"`
		NSP     string            `json:"nsp"`
		ID      *uint64           `json:"id"`
		Data    any               `json:"data"`
	} `json:"decode"`
	Client []struct {
		Wire string  `json:"wire"`
		Type int     `json:"type"`
		NSP  string  `json:"nsp"`
		ID   *uint64 `json:"id"`
		Data any     `json:"data"`
	} `json:"client"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// fromDoc turns {"$bin": base64} markers into []byte.
func fromDoc(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if b, ok := t["$bin"].(string); ok {
			raw, _ := base64.StdEncoding.DecodeString(b)
			return raw
		}
		out := map[string]any{}
		for k, x := range t {
			out[k] = fromDoc(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = fromDoc(x)
		}
		return out
	}
	return v
}

func fromDocs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = fromDoc(a)
	}
	return out
}

// splitHead separates the packet prefix (type, attachments, namespace, id)
// from its JSON so the JSON can be compared structurally: Node keeps object
// keys in insertion order and Go sorts them.
func splitHead(s string) (string, any) {
	i := strings.IndexAny(s, "[{")
	if i < 0 {
		return s, nil
	}
	var v any
	if err := json.Unmarshal([]byte(s[i:]), &v); err != nil {
		panic(err)
	}
	return s[:i], v
}

func assertEncoded(t *testing.T, got []outPacket, want []json.RawMessage) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("packets: got %d want %d", len(got), len(want))
	}
	for i, w := range want {
		var text string
		if json.Unmarshal(w, &text) == nil {
			wantHead, wantData := splitHead(text)
			gotHead, gotData := splitHead(got[i].text)
			if got[i].isBin || wantHead != gotHead || !reflect.DeepEqual(wantData, gotData) {
				t.Errorf("packet %d: got %q want %q", i, got[i].text, text)
			}
			continue
		}
		var doc struct {
			Bin string `json:"$bin"`
		}
		_ = json.Unmarshal(w, &doc)
		raw, _ := base64.StdEncoding.DecodeString(doc.Bin)
		if !got[i].isBin || string(got[i].bin) != string(raw) {
			t.Errorf("packet %d: binary attachment differs", i)
		}
	}
}

func TestEncodeMatchesNodeParser(t *testing.T) {
	v := loadVectors(t)
	for _, e := range v.Events {
		got, err := encodeMessage(e.NSP, e.Event, e.ID, fromDocs(e.Args))
		if err != nil {
			t.Fatal(err)
		}
		assertEncoded(t, got, e.Encoded)
	}
	for _, a := range v.Acks {
		id := a.ID
		got, err := encodeMessage(a.NSP, "", &id, fromDocs(a.Args))
		if err != nil {
			t.Fatal(err)
		}
		assertEncoded(t, got, a.Encoded)
	}
}

func TestDecodeMatchesNodeParser(t *testing.T) {
	v := loadVectors(t)
	for _, d := range v.Decode {
		var head string
		_ = json.Unmarshal(d.Encoded[0], &head)
		p, err := decodeSIO(head)
		if err != nil {
			t.Fatalf("%q: %v", head, err)
		}
		// The Node decoder reports a reassembled BINARY_EVENT as EVENT.
		typ := map[int]int{sioBinaryEvent: sioEvent, sioBinaryAck: sioAck}[p.typ]
		if typ == 0 {
			typ = p.typ
		}
		if typ != d.Type || p.nsp != d.NSP || p.hasID != (d.ID != nil) || (d.ID != nil && p.id != *d.ID) {
			t.Errorf("%q: got %+v", head, p)
		}
		var bins [][]byte
		for _, raw := range d.Encoded[1:] {
			var doc struct {
				Bin string `json:"$bin"`
			}
			_ = json.Unmarshal(raw, &doc)
			b, _ := base64.StdEncoding.DecodeString(doc.Bin)
			bins = append(bins, b)
		}
		if p.attachments != len(bins) {
			t.Errorf("%q: attachments %d want %d", head, p.attachments, len(bins))
		}
		args, err := p.args(bins)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := d.Data.([]any)
		if !reflect.DeepEqual(args, fromDocs(want)) {
			t.Errorf("%q: args %#v want %#v", head, args, fromDocs(want))
		}
	}
}

func TestDecodeClientWireStrings(t *testing.T) {
	for _, c := range loadVectors(t).Client {
		p, err := decodeSIO(c.Wire)
		if err != nil {
			t.Fatalf("%q: %v", c.Wire, err)
		}
		if p.typ != c.Type || p.nsp != c.NSP || p.hasID != (c.ID != nil) || (c.ID != nil && p.id != *c.ID) {
			t.Errorf("%q: got %+v", c.Wire, p)
		}
		if obj, ok := c.Data.(map[string]any); ok {
			var got map[string]any
			if err := json.Unmarshal([]byte(p.data), &got); err != nil || !reflect.DeepEqual(got, obj) {
				t.Errorf("%q: data %q", c.Wire, p.data)
			}
		}
	}
}

func TestDecodeRejectsMalformedPackets(t *testing.T) {
	for _, bad := range []string{"", "9", "x", "2", "2{\"a\":1}", "2[1]", "2[]", "5[\"a\"]", "5x-[\"a\"]", "1junk", "0[1]", "51111111111111111111-[\"a\"]", "299999999999999999999[\"a\"]", "3notjson"} {
		if _, err := decodeSIO(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestPollingPayloadRoundTrip(t *testing.T) {
	pkts := []outPacket{
		{kind: eioMessage, text: `2["a"]`},
		{kind: eioMessage, bin: []byte{1, 2, 3}, isBin: true},
		{kind: eioPing},
	}
	wire := encodePollingPayload(pkts)
	if wire != "42[\"a\"]\x1ebAQID\x1e2" {
		t.Fatalf("wire %q", wire)
	}
	back, err := decodePollingPayload(wire)
	if err != nil || len(back) != 3 || !back[1].isBin || string(back[1].bin) != "\x01\x02\x03" || back[2].kind != eioPing {
		t.Fatalf("%+v %v", back, err)
	}
	for _, bad := range []string{"", "9x", "4a\x1e", "b!!"} {
		if _, err := decodePollingPayload(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
