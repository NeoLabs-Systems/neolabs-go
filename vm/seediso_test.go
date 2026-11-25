package vm

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// readJoliet reads the root directory of the Joliet tree the way a kernel
// does: find the supplementary descriptor, walk its root directory records.
func readJoliet(t *testing.T, image []byte) map[string][]byte {
	t.Helper()
	if string(image[16*isoSector+1:16*isoSector+6]) != "CD001" || image[16*isoSector] != 1 {
		t.Fatal("no primary volume descriptor")
	}
	svd := image[17*isoSector : 18*isoSector]
	if svd[0] != 2 || string(svd[88:91]) != "%/E" {
		t.Fatal("no Joliet supplementary descriptor")
	}
	if image[18*isoSector] != 255 {
		t.Fatal("no terminator")
	}
	if total := binary.LittleEndian.Uint32(svd[80:84]); int(total)*isoSector != len(image) {
		t.Fatalf("volume size %d sectors for %d bytes", total, len(image))
	}
	root := svd[156:190]
	extent := int(binary.LittleEndian.Uint32(root[2:6]))
	length := int(binary.LittleEndian.Uint32(root[10:14]))
	dir := image[extent*isoSector : extent*isoSector+length]
	files := map[string][]byte{}
	for pos := 0; pos < len(dir) && dir[pos] != 0; {
		size := int(dir[pos])
		rec := dir[pos : pos+size]
		idLen := int(rec[32])
		id := rec[33 : 33+idLen]
		if !(idLen == 1 && id[0] <= 1) {
			units := make([]uint16, idLen/2)
			for i := range units {
				units[i] = binary.BigEndian.Uint16(id[2*i:])
			}
			start := int(binary.LittleEndian.Uint32(rec[2:6]))
			n := int(binary.LittleEndian.Uint32(rec[10:14]))
			files[string(utf16.Decode(units))] = image[start*isoSector : start*isoSector+n]
		}
		pos += size
	}
	return files
}

func TestSeedISOCarriesLowerCaseNamesAndExactContent(t *testing.T) {
	big := bytes.Repeat([]byte("0123456789abcdef"), 700) // spans several sectors
	path := filepath.Join(t.TempDir(), "cidata.iso")
	err := WriteSeedISO(path, "CIDATA", []SeedFile{
		{Name: "user-data", Content: big},
		{Name: "meta-data", Content: []byte("instance-id: x\n")},
		{Name: "startup.nsh", Content: []byte("@echo -off\r\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	image, _ := os.ReadFile(path)
	files := readJoliet(t, image)
	if len(files) != 3 || !bytes.Equal(files["user-data"], big) || string(files["meta-data"]) != "instance-id: x\n" || string(files["startup.nsh"]) != "@echo -off\r\n" {
		t.Fatalf("files read back: %v", keys(files))
	}
	if !strings.HasPrefix(string(image[16*isoSector+40:16*isoSector+72]), "CIDATA   ") {
		t.Fatal("volume label is not CIDATA")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestSeedISOIdentifiers(t *testing.T) {
	cases := map[string]string{"user-data": "USER_DATA.;1", "meta-data": "META_DATA.;1", "startup.nsh": "STARTUP.NSH;1"}
	for in, want := range cases {
		if got := string(isoName(in)); got != want {
			t.Errorf("isoName(%q) = %q, want %q", in, got, want)
		}
	}
	if err := WriteSeedISO(filepath.Join(t.TempDir(), "x.iso"), "X", nil); err == nil {
		t.Fatal("an empty seed was accepted")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
