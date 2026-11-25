package vm

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// SeedFile is a file in a cloud-init seed image.
type SeedFile struct {
	Name    string
	Content []byte
}

const isoSector = 2048

// WriteSeedISO writes a small ISO 9660 image with a Joliet tree (so lower-case
// names such as cloud-init's user-data survive) holding files in its root
// directory. It needs no external tool, which keeps a slim container able to
// provision a guest.
func WriteSeedISO(path, volumeID string, files []SeedFile) error {
	if len(files) == 0 {
		return fmt.Errorf("a seed image needs at least one file")
	}
	sorted := append([]SeedFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	const (
		lPathISO    = 19
		mPathISO    = 20
		lPathJoliet = 21
		mPathJoliet = 22
		dirISO      = 23
		dirJoliet   = 24
		firstData   = 25
	)
	now := time.Now().UTC()
	var data [][]byte
	extents := make([]int, len(sorted))
	next := firstData
	for i, f := range sorted {
		extents[i] = next
		data = append(data, f.Content)
		next += max(1, (len(f.Content)+isoSector-1)/isoSector)
	}
	total := next

	isoDir := buildDirectory(dirISO, dirISO, now, sorted, extents, isoName)
	jolietDir := buildDirectory(dirJoliet, dirJoliet, now, sorted, extents, jolietName)
	if len(isoDir) > isoSector || len(jolietDir) > isoSector {
		return fmt.Errorf("too many files for a seed image")
	}

	image := make([]byte, total*isoSector)
	put := func(sector int, b []byte) { copy(image[sector*isoSector:], b) }
	put(16, volumeDescriptor(1, volumeID, total, lPathISO, mPathISO, dirISO, len(isoDir), now, false))
	put(17, volumeDescriptor(2, volumeID, total, lPathJoliet, mPathJoliet, dirJoliet, len(jolietDir), now, true))
	put(18, terminator())
	put(lPathISO, pathTable(dirISO, binary.LittleEndian))
	put(mPathISO, pathTable(dirISO, binary.BigEndian))
	put(lPathJoliet, pathTable(dirJoliet, binary.LittleEndian))
	put(mPathJoliet, pathTable(dirJoliet, binary.BigEndian))
	put(dirISO, isoDir)
	put(dirJoliet, jolietDir)
	for i, content := range data {
		put(extents[i], content)
	}
	return os.WriteFile(path, image, 0o600)
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b[0:4], v)
	binary.BigEndian.PutUint32(b[4:8], v)
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b[0:2], v)
	binary.BigEndian.PutUint16(b[2:4], v)
}

func recordDate(t time.Time) []byte {
	return []byte{byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0}
}

// directoryRecord builds one directory record.
func directoryRecord(identifier []byte, extent, length int, directory bool, t time.Time) []byte {
	size := 33 + len(identifier)
	if size%2 == 1 {
		size++
	}
	r := make([]byte, size)
	r[0] = byte(size)
	both32(r[2:10], uint32(extent))
	both32(r[10:18], uint32(length))
	copy(r[18:25], recordDate(t))
	if directory {
		r[25] = 2
	}
	both16(r[28:32], 1)
	r[32] = byte(len(identifier))
	copy(r[33:], identifier)
	return r
}

func buildDirectory(self, parent int, t time.Time, files []SeedFile, extents []int, name func(string) []byte) []byte {
	out := directoryRecord([]byte{0}, self, isoSector, true, t)
	out = append(out, directoryRecord([]byte{1}, parent, isoSector, true, t)...)
	records := make([][]byte, len(files))
	for i, f := range files {
		records[i] = directoryRecord(name(f.Name), extents[i], len(f.Content), false, t)
	}
	sort.SliceStable(records, func(i, j int) bool { return string(records[i][33:]) < string(records[j][33:]) })
	for _, r := range records {
		out = append(out, r...)
	}
	return out
}

// isoName is a Level 1 style identifier: upper case d-characters, a dot, and a version.
func isoName(name string) []byte {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	id := b.String()
	if !strings.Contains(id, ".") {
		id += "."
	}
	return []byte(id + ";1")
}

func jolietName(name string) []byte { return ucs2(name) }

func ucs2(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(units))
	for i, u := range units {
		binary.BigEndian.PutUint16(out[2*i:], u)
	}
	return out
}

func pathTable(dirExtent int, order binary.ByteOrder) []byte {
	r := make([]byte, isoSector)
	r[0] = 1
	order.PutUint32(r[2:6], uint32(dirExtent))
	order.PutUint16(r[6:8], 1)
	return r
}

func terminator() []byte {
	b := make([]byte, isoSector)
	b[0] = 255
	copy(b[1:6], "CD001")
	b[6] = 1
	return b
}

func padded(s string, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	copy(b, s)
	return b
}

func paddedUCS2(s string, n int) []byte {
	b := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		binary.BigEndian.PutUint16(b[i:], ' ')
	}
	copy(b, ucs2(s))
	return b
}

func volumeTime(t time.Time) []byte {
	return append([]byte(t.Format("20060102150405")+"00"), 0)
}

func volumeDescriptor(kind byte, volumeID string, total, lPath, mPath, rootExtent, rootLength int, now time.Time, joliet bool) []byte {
	b := make([]byte, isoSector)
	b[0] = kind
	copy(b[1:6], "CD001")
	b[6] = 1
	text := func(s string, n int) []byte {
		if joliet {
			return paddedUCS2(s, n)
		}
		return padded(s, n)
	}
	copy(b[8:40], text("", 32))
	copy(b[40:72], text(volumeID, 32))
	both32(b[80:88], uint32(total))
	if joliet {
		copy(b[88:91], "%/E")
	}
	both16(b[120:124], 1)
	both16(b[124:128], 1)
	both16(b[128:132], isoSector)
	both32(b[132:140], 10)
	binary.LittleEndian.PutUint32(b[140:144], uint32(lPath))
	binary.BigEndian.PutUint32(b[148:152], uint32(mPath))
	copy(b[156:190], directoryRecord([]byte{0}, rootExtent, rootLength, true, now))
	for _, field := range [][2]int{{190, 128}, {318, 128}, {446, 128}, {574, 128}} {
		copy(b[field[0]:field[0]+field[1]], text("", field[1]))
	}
	for _, field := range [][2]int{{702, 37}, {739, 37}, {776, 37}} {
		copy(b[field[0]:field[0]+field[1]], text("", field[1]))
	}
	copy(b[813:830], volumeTime(now))
	copy(b[830:847], volumeTime(now))
	copy(b[847:864], append([]byte("0000000000000000"), 0))
	copy(b[864:881], volumeTime(now))
	b[881] = 1
	return b
}
