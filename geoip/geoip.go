// Package geoip looks IP addresses up in the data files of the npm package
// geoip-lite (geoip-city.dat, geoip-city6.dat, geoip-city-names.dat and the
// country fallbacks geoip-country.dat, geoip-country6.dat). It reproduces
// geoip-lite 1.4.x lookups exactly, quirks included: IPv6 ranges are matched
// on their first 64 bits, IPv4-mapped IPv6 addresses use the IPv4 table, and
// the RFC 1918 ranges are never answered.
//
// The data is not embedded (it is about 160 MB). Open points at a directory
// that holds the files, for example node_modules/geoip-lite/data. Files are
// opened on first use and read with ReadAt, so the operating system's page
// cache holds what is hot and the process holds almost nothing.
//
// Licence and attribution: the data is MaxMind GeoLite (GeoLite2) data
// repackaged by geoip-lite (Apache-2.0 code). Using it requires MaxMind's
// attribution: "This product includes GeoLite2 data created by MaxMind,
// available from https://www.maxmind.com", and compliance with the GeoLite
// EULA (CC BY-SA 4.0 for the data). Show that notice wherever the data is
// published or documented.
package geoip

import (
	"errors"
	"io"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	countryRecord4  = 10
	countryRecord6  = 34
	cityRecord4     = 24
	cityRecord6     = 48
	locationRecord  = 88
	noLocation      = 0xffffffff
	nameFile        = "geoip-city-names.dat"
	cityFile4       = "geoip-city.dat"
	cityFile6       = "geoip-city6.dat"
	countryFile4    = "geoip-country.dat"
	countryFile6    = "geoip-country6.dat"
	privateAddr4Cap = 3
)

// Record is what geoip-lite returns for an address.
type Record struct {
	Country  string
	Region   string
	City     string
	Timezone string
	EU       string
	Metro    int32
	Area     uint32
	// LL is latitude and longitude. HasLL is false for an IPv4 range that
	// only knows its country (geoip-lite answers [null, null] there).
	LL    [2]float64
	HasLL bool
}

// table is one data file of fixed-size records, sorted by range.
type table struct {
	f          *os.File
	recordSize int64
	lastLine   int64
}

func openTable(path string, recordSize int64) (*table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() == 0 {
		f.Close()
		return nil, os.ErrNotExist
	}
	return &table{f: f, recordSize: recordSize, lastLine: st.Size()/recordSize - 1}, nil
}

func (t *table) read(line, offset int64, n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := t.f.ReadAt(buf, line*t.recordSize+offset)
	return buf, err
}

func (t *table) u32(line, offset int64) (uint32, error) {
	b, err := t.read(line, offset, 4)
	if err != nil {
		return 0, err
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), nil
}

// DB is an open geoip-lite data directory. It is safe for concurrent use.
type DB struct {
	dir  string
	once sync.Once
	err  error

	v4, v6 *table
	names  *os.File
	// Whether the tables are the city kind (with location ids) or country.
	city4, city6 bool
}

// Open prepares a database over dir. Nothing is read until the first Lookup.
func Open(dir string) *DB { return &DB{dir: dir} }

func (d *DB) load() {
	d.v4, d.city4, d.names = d.openFamily(cityFile4, countryFile4, cityRecord4, countryRecord4)
	d.v6, d.city6, _ = d.openFamily(cityFile6, countryFile6, cityRecord6, countryRecord6)
	if d.v4 == nil && d.v6 == nil {
		d.err = errors.New("geoip: no data files in " + d.dir)
	}
}

// openFamily opens the city table, falling back to the country table the way
// geoip-lite does. For IPv4 it also opens the location names.
func (d *DB) openFamily(cityName, countryName string, citySize, countrySize int64) (*table, bool, *os.File) {
	if t, err := openTable(filepath.Join(d.dir, cityName), citySize); err == nil {
		names, _ := os.Open(filepath.Join(d.dir, nameFile))
		if cityName == cityFile4 && names == nil {
			t.f.Close()
		} else {
			return t, true, names
		}
	}
	t, err := openTable(filepath.Join(d.dir, countryName), countrySize)
	if err != nil {
		return nil, false, nil
	}
	return t, false, nil
}

// Close releases the open files.
func (d *DB) Close() error {
	for _, f := range []*os.File{tableFile(d.v4), tableFile(d.v6), d.names} {
		if f != nil {
			f.Close()
		}
	}
	return nil
}

func tableFile(t *table) *os.File {
	if t == nil {
		return nil
	}
	return t.f
}

// Lookup returns the record for ip (text form, as net.isIP accepts it). ok is
// false when the address is unknown, malformed or private. An error means the
// data files are unusable.
func (d *DB) Lookup(ip string) (rec Record, ok bool, err error) {
	d.once.Do(d.load)
	if d.err != nil {
		return Record{}, false, d.err
	}
	if ip == "" {
		return Record{}, false, nil
	}
	addr, perr := netip.ParseAddr(ip)
	if perr != nil {
		return Record{}, false, nil
	}
	if addr.Is4() {
		return d.lookup4(aton4(ip))
	}
	if v4, mapped := get4mapped(ip); mapped {
		return d.lookup4(aton4(v4))
	}
	return d.lookup6(aton6(ip))
}

var privateRanges4 = [privateAddr4Cap][2]uint32{
	{10 << 24, 10<<24 | 0xffffff},
	{172<<24 | 16<<16, 172<<24 | 31<<16 | 0xffff},
	{192<<24 | 168<<16, 192<<24 | 168<<16 | 0xffff},
}

func (d *DB) lookup4(ip uint32) (Record, bool, error) {
	t := d.v4
	if t == nil {
		return Record{}, false, nil
	}
	first, err := t.u32(0, 0)
	if err != nil {
		return Record{}, false, err
	}
	last, err := t.u32(t.lastLine, 4)
	if err != nil {
		return Record{}, false, err
	}
	if ip > last || ip < first {
		return Record{}, false, nil
	}
	for _, r := range privateRanges4 {
		if ip >= r[0] && ip <= r[1] {
			return Record{}, false, nil
		}
	}
	line, found, err := search(t.lastLine, func(line int64) (int, error) {
		floor, err := t.u32(line, 0)
		if err != nil {
			return 0, err
		}
		ceil, err := t.u32(line, 4)
		if err != nil {
			return 0, err
		}
		return compare(floor <= ip, ceil >= ip, floor > ip), nil
	})
	if err != nil || !found {
		return Record{}, false, err
	}
	if !d.city4 {
		b, err := t.read(line, 8, 2)
		return Record{Country: string(b)}, true, err
	}
	locID, err := t.u32(line, 8)
	if err != nil {
		return Record{}, false, err
	}
	rec := Record{}
	if locID != noLocation {
		if err := d.fillLocation(&rec, locID); err != nil {
			return Record{}, false, err
		}
		lat, err := t.u32(line, 12)
		if err != nil {
			return Record{}, false, err
		}
		lon, err := t.u32(line, 16)
		if err != nil {
			return Record{}, false, err
		}
		area, err := t.u32(line, 20)
		if err != nil {
			return Record{}, false, err
		}
		rec.LL = [2]float64{float64(int32(lat)) / 10000, float64(int32(lon)) / 10000}
		rec.HasLL = true
		rec.Area = area
	}
	return rec, true, nil
}

func (d *DB) lookup6(ip [2]uint32) (Record, bool, error) {
	t := d.v6
	if t == nil {
		return Record{}, false, nil
	}
	readip := func(line int64, offset int64) ([2]uint32, error) {
		var out [2]uint32
		for i := range out {
			v, err := t.u32(line, offset*16+int64(i)*4)
			if err != nil {
				return out, err
			}
			out[i] = v
		}
		return out, nil
	}
	last, err := readip(t.lastLine, 1)
	if err != nil {
		return Record{}, false, err
	}
	first, err := readip(0, 0)
	if err != nil {
		return Record{}, false, err
	}
	if cmp6(ip, last) > 0 || cmp6(ip, first) < 0 {
		return Record{}, false, nil
	}
	line, found, err := search(t.lastLine, func(line int64) (int, error) {
		floor, err := readip(line, 0)
		if err != nil {
			return 0, err
		}
		ceil, err := readip(line, 1)
		if err != nil {
			return 0, err
		}
		return compare(cmp6(floor, ip) <= 0, cmp6(ceil, ip) >= 0, cmp6(floor, ip) > 0), nil
	})
	if err != nil || !found {
		return Record{}, false, err
	}
	rec := Record{HasLL: true}
	if !d.city6 {
		b, err := t.read(line, 32, 2)
		rec.Country = cutNUL(b)
		return rec, true, err
	}
	locID, err := t.u32(line, 32)
	if err != nil {
		return Record{}, false, err
	}
	if locID != noLocation {
		if err := d.fillLocation(&rec, locID); err != nil {
			return Record{}, false, err
		}
		lat, err := t.u32(line, 36)
		if err != nil {
			return Record{}, false, err
		}
		lon, err := t.u32(line, 40)
		if err != nil {
			return Record{}, false, err
		}
		area, err := t.u32(line, 44)
		if err != nil {
			return Record{}, false, err
		}
		rec.LL = [2]float64{float64(int32(lat)) / 10000, float64(int32(lon)) / 10000}
		rec.Area = area
	}
	return rec, true, nil
}

func (d *DB) fillLocation(rec *Record, locID uint32) error {
	buf := make([]byte, locationRecord)
	if d.names == nil {
		return errors.New("geoip: location names missing")
	}
	if _, err := d.names.ReadAt(buf, int64(locID)*locationRecord); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	rec.Country = cutNUL(buf[0:2])
	rec.Region = cutNUL(buf[2:5])
	rec.Metro = int32(uint32(buf[5])<<24 | uint32(buf[6])<<16 | uint32(buf[7])<<8 | uint32(buf[8]))
	rec.EU = cutNUL(buf[9:10])
	rec.Timezone = cutNUL(buf[10:42])
	rec.City = cutNUL(buf[42:88])
	return nil
}

// cutNUL decodes like Buffer.toString('utf8') followed by replace(/\0.*/, ”):
// invalid bytes become U+FFFD and the text ends at the first NUL.
func cutNUL(b []byte) string {
	s := strings.ToValidUTF8(string(b), "�")
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return s
}

// compare turns the three range tests into -1 (go lower), 0 (hit), 1 (go
// higher) for search.
func compare(floorOK, ceilOK, floorAbove bool) int {
	switch {
	case floorOK && ceilOK:
		return 0
	case floorAbove:
		return -1
	}
	return 1
}

// search is geoip-lite's binary search, step for step (it differs from the
// textbook one near the ends, and results must match it).
func search(lastLine int64, probe func(line int64) (int, error)) (int64, bool, error) {
	fline, cline := int64(0), lastLine
	for {
		line := (cline-fline+1)/2 + fline // Math.round rounds halves up
		res, err := probe(line)
		if err != nil {
			return 0, false, err
		}
		switch {
		case res == 0:
			return line, true, nil
		case fline == cline:
			return 0, false, nil
		case fline == cline-1:
			if line == fline {
				fline = cline
			} else {
				cline = fline
			}
		case res < 0:
			cline = line
		default:
			fline = line
		}
	}
}

func cmp6(a, b [2]uint32) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// aton4 is utils.aton4 (parseInt of each dotted part).
func aton4(s string) uint32 {
	parts := strings.Split(s, ".")
	var n uint32
	for i := 0; i < 4; i++ {
		n += uint32(parseInt(partAt(parts, i), 10)) << (24 - 8*uint(i))
	}
	return n
}

func partAt(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return ""
}

// parseInt is JavaScript's parseInt for non-negative input: the longest valid
// prefix, 0 when there is none (NaN behaves as 0 in the callers' bit math).
func parseInt(s string, base int) uint64 {
	s = strings.TrimLeft(s, " \t\n\r")
	var n uint64
	for _, c := range s {
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return n
		}
		if v >= base {
			return n
		}
		n = n*uint64(base) + uint64(v)
		if n > math.MaxUint32 {
			n &= math.MaxUint32
		}
	}
	return n
}

// aton6 is utils.aton6, including the way it expands "::" (it leaves stale
// groups behind for some addresses, and the data lookup is keyed on its
// result, so the behaviour is kept).
func aton6(s string) [2]uint32 {
	s = strings.ReplaceAll(s, `"`, "")
	a := strings.Split(s, ":")
	l := len(a) - 1
	if a[l] == "" {
		a[l] = "0"
	}
	if l < 7 {
		grown := make([]string, 8)
		copy(grown, a)
		a = grown
		for i := l; i >= 0 && a[i] != ""; i-- {
			a[7-l+i] = a[i]
		}
	}
	var w [8]uint32
	for i := 0; i < 8; i++ {
		if a[i] != "" && a[i] != "0" {
			w[i] = uint32(parseInt(a[i], 16))
		}
	}
	var r [2]uint32
	for i := range r {
		r[i] = w[2*i]<<16 + w[2*i+1]
	}
	return r
}

// get4mapped returns the IPv4 part of an IPv4-mapped IPv6 address.
func get4mapped(ip string) (string, bool) {
	up := strings.ToUpper(ip)
	for _, prefix := range []string{"0:0:0:0:0:FFFF:", "::FFFF:"} {
		if strings.HasPrefix(up, prefix) {
			return up[len(prefix):], true
		}
	}
	return "", false
}
