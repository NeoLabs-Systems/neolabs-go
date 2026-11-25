package filebox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type chunkedReader struct {
	data  []byte
	pos   int
	sizes []int
	idx   int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	sz := 7 // intentionally odd/unaligned
	if len(c.sizes) > 0 {
		sz = c.sizes[c.idx%len(c.sizes)]
		c.idx++
	}
	if sz > len(p) {
		sz = len(p)
	}
	rem := len(c.data) - c.pos
	if sz > rem {
		sz = rem
	}
	copy(p, c.data[c.pos:c.pos+sz])
	c.pos += sz
	if c.pos == len(c.data) {
		return sz, io.EOF
	}
	return sz, nil
}

func TestSealAndOpenMatchesStdlibGCM(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	magic := [4]byte{'N', 'R', 'F', '1'}

	for _, length := range []int{0, 1, 15, 16, 17, 31, 32, 33, 1023, 65537, 300000} {
		plain := make([]byte, length)
		_, _ = rand.Read(plain)

		var out bytes.Buffer
		cr := &chunkedReader{data: plain, sizes: []int{1, 5, 16, 17, 13, 300}}
		if err := SealReader(&out, cr, key, magic); err != nil {
			t.Fatalf("len %d SealReader: %v", length, err)
		}
		sealed := out.Bytes()
		if len(sealed) != 4+IVSize+length+TagSize {
			t.Fatalf("len %d unexpected sealed len %d", length, len(sealed))
		}

		// Verify stdlib AES-GCM opens what SealReader wrote.
		block, _ := aes.NewCipher(key)
		aead, _ := cipher.NewGCM(block)
		iv := sealed[4 : 4+IVSize]
		body := sealed[4+IVSize:] // ciphertext || tag
		got, err := aead.Open(nil, iv, body, nil)
		if err != nil {
			t.Fatalf("len %d stdlib aead.Open failed: %v", length, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("len %d plaintext mismatch", length)
		}

		// Write stdlib-sealed payload to disk and open via filebox.
		dir := t.TempDir()
		src := filepath.Join(dir, "std.nrf1")
		dst := filepath.Join(dir, "plain.bin")
		var stdOut bytes.Buffer
		stdOut.Write(magic[:])
		stdOut.Write(iv)
		stdOut.Write(aead.Seal(nil, iv, plain, nil))
		if err := os.WriteFile(src, stdOut.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if !HasMagic(src, magic) {
			t.Fatal("HasMagic false")
		}
		if err := DecryptFile(src, dst, key, magic); err != nil {
			t.Fatalf("len %d DecryptFile: %v", length, err)
		}
		dec, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dec, plain) {
			t.Fatalf("len %d decrypted mismatch", length)
		}

		// Tamper test: flipping one byte must fail and remove dst.
		tampered := filepath.Join(dir, "tampered.nrf1")
		bad := append([]byte(nil), stdOut.Bytes()...)
		bad[len(bad)-1] ^= 0xff
		_ = os.WriteFile(tampered, bad, 0o600)
		badDst := filepath.Join(dir, "should-not-exist.bin")
		if err := DecryptFile(tampered, badDst, key, magic); err == nil {
			t.Fatalf("expected tamper failure for len %d", length)
		}
		if _, err := os.Stat(badDst); !os.IsNotExist(err) {
			t.Fatalf("expected failed DecryptFile to remove %s", badDst)
		}
	}

	// File seal convenience check
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.txt")
	sealedPath := filepath.Join(dir, "out.nrb1")
	_ = os.WriteFile(inPath, []byte(strings.Repeat("neorecall ", 5000)), 0o600)
	if err := SealFile(inPath, sealedPath, key, [4]byte{'N', 'R', 'B', '1'}); err != nil {
		t.Fatal(err)
	}
	gotBytes, err := DecryptBytes(sealedPath, key, [4]byte{'N', 'R', 'B', '1'})
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBytes) != strings.Repeat("neorecall ", 5000) {
		t.Fatal("DecryptBytes mismatch")
	}
}
