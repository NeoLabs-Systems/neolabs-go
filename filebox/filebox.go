// Package filebox provides streaming AES-256-GCM encryption and decryption for
// files prefixed with a 4-byte magic header (e.g. NRF1 sealed chunks, NRB1 backups).
//
// Wire layout: magic(4) | iv(12) | ciphertext(N) | tag(16), identical to
// Node's crypto.createCipheriv('aes-256-gcm') stream pipelines with O(1) memory.
package filebox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	IVSize  = 12
	TagSize = 16
	bufSize = 256 * 1024
)

var (
	ErrInvalidMagic  = errors.New("filebox: unrecognized file magic")
	ErrTruncated     = errors.New("filebox: encrypted file is truncated")
	ErrAuthFailed    = errors.New("filebox: authentication failed")
	ErrInvalidKeyLen = errors.New("filebox: key must be 32 bytes for AES-256-GCM")
)

// ghash implements NIST SP 800-38D GHASH in GF(2^128) with irreducible polynomial
// x^128 + x^7 + x^2 + x + 1, matching standard AES-GCM bit ordering (reflected).
type ghash struct {
	h [16]byte
	y [16]byte
}

func (g *ghash) init(block cipher.Block) {
	var zero [16]byte
	block.Encrypt(g.h[:], zero[:])
}

// mul multiplies x by h in GF(2^128) (NIST SP 800-38D Algorithm 1).
func (g *ghash) mul(x *[16]byte) {
	var z [16]byte
	v := g.h
	for i := 0; i < 16; i++ {
		xi := x[i]
		for bit := 7; bit >= 0; bit-- {
			mask := byte(-int8((xi >> bit) & 1))
			for k := 0; k < 16; k++ {
				z[k] ^= v[k] & mask
			}
			lsb := v[15] & 1
			for k := 15; k > 0; k-- {
				v[k] = (v[k] >> 1) | ((v[k-1] & 1) << 7)
			}
			v[0] >>= 1
			// Reduction polynomial R = 0xe1 || 0^120
			v[0] ^= 0xe1 & byte(-int8(lsb))
		}
	}
	*x = z
}

func (g *ghash) update(data []byte) {
	for len(data) >= 16 {
		for i := 0; i < 16; i++ {
			g.y[i] ^= data[i]
		}
		g.mul(&g.y)
		data = data[16:]
	}
	if len(data) > 0 {
		for i := range data {
			g.y[i] ^= data[i]
		}
		g.mul(&g.y)
	}
}

func (g *ghash) finalize(cipherLen uint64, j0Mask *[16]byte, tag *[16]byte) {
	var lenBlock [16]byte
	// AAD is empty (8 zero bytes), followed by bit length of ciphertext.
	binary.BigEndian.PutUint64(lenBlock[8:], cipherLen*8)
	g.update(lenBlock[:])
	for i := 0; i < 16; i++ {
		tag[i] = g.y[i] ^ j0Mask[i]
	}
}

// newStreamState sets up AES-256-CTR (starting at J0+1) and GHASH for a 12-byte IV.
func newStreamState(key, iv []byte) (cipher.Stream, *ghash, [16]byte, error) {
	if len(key) != 32 {
		return nil, nil, [16]byte{}, ErrInvalidKeyLen
	}
	if len(iv) != IVSize {
		return nil, nil, [16]byte{}, fmt.Errorf("filebox: iv must be %d bytes", IVSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, [16]byte{}, err
	}
	var j0 [16]byte
	copy(j0[:12], iv)
	j0[15] = 1

	var j0Mask [16]byte
	block.Encrypt(j0Mask[:], j0[:])

	var ctrStart [16]byte
	copy(ctrStart[:], j0[:])
	ctrStart[15] = 2 // J0 + 1 for first payload block

	var gh ghash
	gh.init(block)
	return cipher.NewCTR(block, ctrStart[:]), &gh, j0Mask, nil
}

// SealReader streams r to w with magic || iv || ciphertext || tag using O(1) memory.
func SealReader(w io.Writer, r io.Reader, key []byte, magic [4]byte) error {
	iv := make([]byte, IVSize)
	if _, err := rand.Read(iv); err != nil {
		return fmt.Errorf("filebox: generate iv: %w", err)
	}
	ctr, gh, j0Mask, err := newStreamState(key, iv)
	if err != nil {
		return err
	}
	var hdr [4 + IVSize]byte
	copy(hdr[:4], magic[:])
	copy(hdr[4:], iv)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}

	buf := make([]byte, bufSize)
	var total uint64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			// Feed GHASH in 16-byte aligned chunks except possibly the final read;
			// accumulate a small tail buffer if n is not a multiple of 16.
			if total%16 != 0 {
				return errors.New("filebox: internal unaligned non-final block")
			}
			full := (n / 16) * 16
			ctr.XORKeyStream(buf[:n], buf[:n])
			if full > 0 {
				gh.update(buf[:full])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			total += uint64(n)
			if full < n {
				// Partial block can only occur validly on EOF unless an intermediate read was unaligned.
				// Wait until next read by shifting or require EOF — read up to fill a 16B boundary!
				rem := buf[full:n]
				if rerr == io.EOF {
					gh.update(rem)
					break
				}
				// Reconcile unaligned read boundary by reading small tail to hit 16 bytes cleanly.
				var blockTail [16]byte
				copy(blockTail[:], rem)
				have := len(rem)
				for have < 16 {
					var one [16]byte
					m, merr := r.Read(one[:16-have])
					if m > 0 {
						ctr.XORKeyStream(one[:m], one[:m])
						if _, werr := w.Write(one[:m]); werr != nil {
							return werr
						}
						copy(blockTail[have:], one[:m])
						have += m
						total += uint64(m)
					}
					if merr == io.EOF {
						rerr = io.EOF
						break
					}
					if merr != nil {
						return merr
					}
				}
				gh.update(blockTail[:have])
				if rerr == io.EOF {
					break
				}
				continue
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	var tag [TagSize]byte
	gh.finalize(total, &j0Mask, &tag)
	_, err = w.Write(tag[:])
	return err
}

// SealFile encrypts srcPath into dstPath atomically-named with 0600 permissions.
func SealFile(srcPath, dstPath string, key []byte, magic [4]byte) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := SealReader(out, in, key, magic); err != nil {
		out.Close()
		_ = os.Remove(dstPath)
		return err
	}
	return out.Close()
}

// OpenFile decrypts and authenticates an encrypted file into w. Authentication
// runs before final success; callers writing files must remove partial output
// if OpenFile returns an error.
func OpenFile(srcPath string, w io.Writer, key []byte, magic [4]byte) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	st, err := in.Stat()
	if err != nil {
		return err
	}
	minSize := int64(4 + IVSize + TagSize)
	if st.Size() < minSize {
		return ErrTruncated
	}
	var hdr [4 + IVSize]byte
	if _, err := io.ReadFull(in, hdr[:]); err != nil {
		return err
	}
	if !bytes.Equal(hdr[:4], magic[:]) {
		return ErrInvalidMagic
	}
	ctr, gh, j0Mask, err := newStreamState(key, hdr[4:])
	if err != nil {
		return err
	}

	cipherLen := st.Size() - minSize
	buf := make([]byte, bufSize)
	remaining := cipherLen
	for remaining > 0 {
		want := int64(len(buf))
		if want > remaining {
			want = remaining
		}
		n, rerr := io.ReadFull(in, buf[:want])
		if rerr != nil {
			return rerr
		}
		gh.update(buf[:n])
		ctr.XORKeyStream(buf[:n], buf[:n])
		if _, werr := w.Write(buf[:n]); werr != nil {
			return werr
		}
		remaining -= int64(n)
	}

	var wantTag [TagSize]byte
	if _, err := io.ReadFull(in, wantTag[:]); err != nil {
		return err
	}
	var gotTag [TagSize]byte
	gh.finalize(uint64(cipherLen), &j0Mask, &gotTag)
	if subtle.ConstantTimeCompare(gotTag[:], wantTag[:]) != 1 {
		return ErrAuthFailed
	}
	return nil
}

// DecryptFile writes the verified plaintext of srcPath to dstPath (0600). On
// authentication failure or read/write error, dstPath is removed.
func DecryptFile(srcPath, dstPath string, key []byte, magic [4]byte) error {
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = OpenFile(srcPath, out, key, magic)
	cerr := out.Close()
	if err != nil {
		_ = os.Remove(dstPath)
		return err
	}
	if cerr != nil {
		_ = os.Remove(dstPath)
		return cerr
	}
	return nil
}

// DecryptBytes returns the verified plaintext of srcPath in memory.
func DecryptBytes(srcPath string, key []byte, magic [4]byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := OpenFile(srcPath, &buf, key, magic); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// HasMagic reports whether path begins with the 4-byte magic header.
func HasMagic(path string, magic [4]byte) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false
	}
	return bytes.Equal(hdr[:], magic[:])
}
