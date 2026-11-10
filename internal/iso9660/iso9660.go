// Package iso9660 writes a minimal ISO 9660 (ECMA-119) image: one volume, a
// root directory holding regular files, no subdirectories, and no Joliet or
// Rock Ridge extensions. It's what separate seed media needs (#242): IncusOS
// mounts a volume labelled SEED_DATA read-only as vfat, then iso9660, and
// reads its seed files from the root.
//
// Names are stored the way plain ISO 9660 spells them, upper case with a
// ";1" version ("INSTALL.YAML;1"). Linux's isofs, at its default map=normal,
// reads such a name back as lower case with the version dropped
// ("install.yaml"), which is the name incus-osd opens.
//
// The whole image is built from memory and streamed to an io.Writer, so a
// file's contents never touch the disk on their way in: the caller's seed
// carries a node's WireGuard private key.
package iso9660

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// SectorSize is ISO 9660's logical block size; every structure starts on one.
const SectorSize = 2048

// systemAreaSectors is the 32 KiB ahead of the first volume descriptor that
// ISO 9660 leaves to the system (zeros here).
const systemAreaSectors = 16

// maxNameLen bounds a file identifier, name and extension together but
// without the ";1" version (ECMA-119 level 2, 7.5.1).
const maxNameLen = 30

// File is one regular file in the image's root directory.
type File struct {
	// Name is the file's name, e.g. "install.yaml". It's stored upper case,
	// so it may use only letters, digits and "_", with at most one ".".
	Name string
	Data []byte
}

// Options are the image's volume-level fields.
type Options struct {
	// VolumeID is the volume identifier, which Linux exposes as the
	// filesystem label (/dev/disk/by-label/<VolumeID>). At most 32 of
	// A-Z, 0-9 and "_".
	VolumeID string
	// ModTime stamps the volume and every file. The zero time leaves the
	// dates unspecified, which makes the image byte-for-byte reproducible.
	ModTime time.Time
}

// entry is a File laid out on the image.
type entry struct {
	id     string // the stored identifier, e.g. "INSTALL.YAML;1"
	data   []byte
	extent uint32 // first sector, or 0 for an empty file
}

// Write writes an ISO 9660 image holding files at its root to w. It checks
// every name before writing anything, so a bad name leaves w untouched.
func Write(w io.Writer, opts Options, files []File) error {
	if err := checkDChars("volume ID", opts.VolumeID, 32); err != nil {
		return err
	}
	entries, err := layoutEntries(files)
	if err != nil {
		return err
	}

	// Sectors 16 and 17 are the primary volume descriptor and the set
	// terminator; 18 and 19 the little- and big-endian path tables (one
	// 10-byte record each, for the root); then the root directory, then the
	// files' data, each from a new sector.
	const (
		pvdSector    = systemAreaSectors
		lPathSector  = pvdSector + 2
		mPathSector  = lPathSector + 1
		rootDirStart = mPathSector + 1
	)
	rootDir := dirSectors(entries, opts.ModTime, rootDirStart)
	next := uint32(rootDirStart) + uint32(len(rootDir)/SectorSize) //nolint:gosec // G115: the root directory is a few sectors
	for i := range entries {
		n := sectorsFor(len(entries[i].data))
		if n == 0 {
			continue
		}
		entries[i].extent = next
		next += n
	}
	// The directory's records point at the files' extents, so lay it out
	// again now that they're known; its size doesn't depend on them.
	rootDir = dirSectors(entries, opts.ModTime, rootDirStart)
	totalSectors := next

	var buf bytes.Buffer
	buf.Grow(int(totalSectors) * SectorSize)
	buf.Write(make([]byte, systemAreaSectors*SectorSize))
	buf.Write(primaryVolumeDescriptor(opts, totalSectors, rootDirStart, uint32(len(rootDir)), lPathSector, mPathSector)) //nolint:gosec // G115: the root directory is a few sectors
	buf.Write(terminator())
	buf.Write(pathTable(binary.LittleEndian, rootDirStart))
	buf.Write(pathTable(binary.BigEndian, rootDirStart))
	buf.Write(rootDir)
	for _, e := range entries {
		buf.Write(padSector(e.data))
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write ISO image: %w", err)
	}
	return nil
}

// layoutEntries checks and converts files' names, sorted as ISO 9660 orders
// a directory's records.
func layoutEntries(files []File) ([]entry, error) {
	entries := make([]entry, 0, len(files))
	seen := make(map[string]string, len(files))
	for _, f := range files {
		id, err := fileID(f.Name)
		if err != nil {
			return nil, err
		}
		if prev, ok := seen[id]; ok {
			return nil, fmt.Errorf("file names %q and %q are the same on ISO 9660 (%s)", prev, f.Name, id)
		}
		seen[id] = f.Name
		if uint64(len(f.Data)) > math.MaxUint32 {
			return nil, fmt.Errorf("file %q is %d bytes, more than ISO 9660 holds in one extent", f.Name, len(f.Data))
		}
		entries = append(entries, entry{id: id, data: f.Data})
	}
	sort.Slice(entries, func(i, j int) bool { return idLess(entries[i].id, entries[j].id) })
	return entries, nil
}

// idLess orders two "NAME.EXT;1" identifiers as ECMA-119 9.3 does: by name,
// then by extension, each compared as if space-padded to the longer one's
// length. That isn't the identifiers' raw byte order: ';' (0x3B) sorts after
// the digits, so raw order puts "A.B1;1" before "A.B;1", where 9.3 puts
// "A.B" first (its padding space, 0x20, sorts before '1'). Every version is
// 1, so 9.3's last key never decides.
func idLess(a, b string) bool {
	aName, aExt := splitID(a)
	bName, bExt := splitID(b)
	if c := comparePadded(aName, bName); c != 0 {
		return c < 0
	}
	return comparePadded(aExt, bExt) < 0
}

// splitID splits "NAME.EXT;1" into NAME and EXT.
func splitID(id string) (name, ext string) {
	id, _, _ = strings.Cut(id, ";")
	name, ext, _ = strings.Cut(id, ".")
	return name, ext
}

// comparePadded compares a and b with the shorter padded with spaces.
func comparePadded(a, b string) int {
	n := max(len(a), len(b))
	return strings.Compare(a+strings.Repeat(" ", n-len(a)), b+strings.Repeat(" ", n-len(b)))
}

// fileID is name's ISO 9660 file identifier: upper case, a "." always
// present (ECMA-119 7.5.1), and the ";1" version.
func fileID(name string) (string, error) {
	upper := strings.ToUpper(name)
	base, ext, _ := strings.Cut(upper, ".")
	if base == "" && ext == "" {
		return "", errors.New("empty file name")
	}
	if len(base)+len(ext) > maxNameLen {
		return "", fmt.Errorf("file name %q is longer than ISO 9660's %d characters", name, maxNameLen)
	}
	for _, part := range []string{base, ext} {
		if err := checkDChars(fmt.Sprintf("file name %q", name), part, maxNameLen); err != nil {
			return "", err
		}
	}
	return base + "." + ext + ";1", nil
}

// checkDChars reports whether s uses only ISO 9660 d-characters (A-Z, 0-9,
// "_"), compared case-insensitively, and fits in n.
func checkDChars(what, s string, n int) error {
	if len(s) > n {
		return fmt.Errorf("%s is longer than %d characters", what, n)
	}
	for _, r := range strings.ToUpper(s) {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return fmt.Errorf("%s has %q; ISO 9660 allows only letters, digits and _", what, r)
		}
	}
	return nil
}

func sectorsFor(n int) uint32 {
	return uint32((n + SectorSize - 1) / SectorSize) //nolint:gosec // G115: layoutEntries bounds n to MaxUint32
}

func padSector(b []byte) []byte {
	if rem := len(b) % SectorSize; rem != 0 {
		return append(b[:len(b):len(b)], make([]byte, SectorSize-rem)...)
	}
	return b
}

// dirSectors is the root directory's extent starting at sector start: "."
// and ".." (both the root itself), then one record per entry. A record never
// spans a sector boundary (ECMA-119 6.8.1.1), so one that doesn't fit starts
// the next sector.
func dirSectors(entries []entry, mod time.Time, start uint32) []byte {
	records := make([][]byte, 0, len(entries)+2)
	records = append(records, nil, nil) // "." and "..", once the size is known
	for _, e := range entries {
		records = append(records, dirRecord([]byte(e.id), e.extent, uint32(len(e.data)), false, mod)) //nolint:gosec // G115: layoutEntries bounds it
	}

	layout := func() []byte {
		var out []byte
		used := 0
		for _, r := range records {
			if used+len(r) > SectorSize {
				out = append(out, make([]byte, SectorSize-used)...)
				used = 0
			}
			out = append(out, r...)
			used += len(r)
		}
		return padSector(out)
	}

	// "." and ".." are 34 bytes whatever the size, so a placeholder pass
	// gives the size they record.
	self := func(size uint32) {
		records[0] = dirRecord([]byte{0}, start, size, true, mod)
		records[1] = dirRecord([]byte{1}, start, size, true, mod)
	}
	self(0)
	size := uint32(len(layout())) //nolint:gosec // G115: a few sectors
	self(size)
	return layout()
}

// dirRecord is one directory record (ECMA-119 9.1).
func dirRecord(id []byte, extent, size uint32, dir bool, mod time.Time) []byte {
	n := 33 + len(id)
	if n%2 == 1 {
		n++ // padding: a record's length is even
	}
	r := make([]byte, n)
	r[0] = byte(n) //nolint:gosec // G115: fileID caps an identifier at 33 bytes, so n <= 66
	putBoth32(r[2:10], extent)
	putBoth32(r[10:18], size)
	copy(r[18:25], recordTime(mod))
	if dir {
		r[25] = 0x02
	}
	putBoth16(r[28:32], 1) // volume sequence number
	r[32] = byte(len(id))  //nolint:gosec // G115: fileID caps an identifier at 33 bytes
	copy(r[33:], id)
	return r
}

// recordTime is a directory record's 7-byte date (ECMA-119 9.1.5), in UTC;
// all zeros when t is zero.
func recordTime(t time.Time) []byte {
	if t.IsZero() {
		return make([]byte, 7)
	}
	t = t.UTC()
	return []byte{byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0} //nolint:gosec // G115: calendar fields fit a byte for 1900-2155
}

// volumeTime is a volume descriptor's 17-byte date (ECMA-119 8.4.26.1), in
// UTC; "not specified" (sixteen '0's and a zero offset) when t is zero.
func volumeTime(t time.Time) []byte {
	if t.IsZero() {
		return append([]byte(strings.Repeat("0", 16)), 0)
	}
	return append([]byte(t.UTC().Format("20060102150405")+"00"), 0)
}

// primaryVolumeDescriptor is the primary volume descriptor (ECMA-119 8.4).
func primaryVolumeDescriptor(opts Options, totalSectors, rootExtent, rootSize, lPath, mPath uint32) []byte {
	d := make([]byte, SectorSize)
	d[0] = 1 // primary
	copy(d[1:6], "CD001")
	d[6] = 1
	copy(d[8:40], spaces(32))
	copy(d[40:72], padRight(strings.ToUpper(opts.VolumeID), 32))
	putBoth32(d[80:88], totalSectors)
	putBoth16(d[120:124], 1) // volume set size
	putBoth16(d[124:128], 1) // volume sequence number
	putBoth16(d[128:132], SectorSize)
	putBoth32(d[132:140], 10) // path table size: the root's one record
	binary.LittleEndian.PutUint32(d[140:144], lPath)
	binary.BigEndian.PutUint32(d[148:152], mPath)
	copy(d[156:190], dirRecord([]byte{0}, rootExtent, rootSize, true, opts.ModTime))
	// Volume set, publisher, data preparer and application identifiers,
	// then the copyright, abstract and bibliographic file identifiers.
	copy(d[190:813], spaces(813-190))
	copy(d[813:830], volumeTime(opts.ModTime)) // creation
	copy(d[830:847], volumeTime(opts.ModTime)) // modification
	copy(d[847:864], volumeTime(time.Time{}))  // expiration
	copy(d[864:881], volumeTime(time.Time{}))  // effective
	d[881] = 1                                 // file structure version
	return d
}

// terminator is the volume descriptor set terminator (ECMA-119 8.3).
func terminator() []byte {
	d := make([]byte, SectorSize)
	d[0] = 255
	copy(d[1:6], "CD001")
	d[6] = 1
	return d
}

// pathTable is a path table holding only the root (ECMA-119 9.4), in one
// sector, in order's byte order.
func pathTable(order binary.ByteOrder, rootExtent uint32) []byte {
	t := make([]byte, SectorSize)
	t[0] = 1 // identifier length
	order.PutUint32(t[2:6], rootExtent)
	order.PutUint16(t[6:8], 1) // parent: the root is its own
	// t[8] is the root's identifier, 0x00; t[9] pads it to even.
	return t
}

// putBoth32 writes v little-endian then big-endian, ISO 9660's "both-byte
// order" (7.3.3).
func putBoth32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b[0:4], v)
	binary.BigEndian.PutUint32(b[4:8], v)
}

// putBoth16 is putBoth32 for a 16-bit field (7.2.3).
func putBoth16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b[0:2], v)
	binary.BigEndian.PutUint16(b[2:4], v)
}

func spaces(n int) []byte { return bytes.Repeat([]byte{' '}, n) }

func padRight(s string, n int) []byte {
	b := spaces(n)
	copy(b, s)
	return b
}
