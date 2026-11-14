package iso9660

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readBack is a minimal ISO 9660 reader, independent of the writer's own
// helpers: the volume ID and the root directory's files, named as Linux's
// isofs shows them at its default map=normal (lower case, ";1" dropped).
type readBack struct {
	volumeID string
	files    map[string][]byte
	order    []string // stored identifiers, in directory order
}

func read(t *testing.T, img []byte) readBack {
	t.Helper()
	if len(img)%SectorSize != 0 {
		t.Fatalf("image is %d bytes, not a whole number of %d-byte sectors", len(img), SectorSize)
	}
	sector := func(n uint32) []byte {
		off := int(n) * SectorSize
		if off+SectorSize > len(img) {
			t.Fatalf("sector %d is past the image's end (%d bytes)", n, len(img))
		}
		return img[off : off+SectorSize]
	}
	pvd := sector(16)
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" || pvd[6] != 1 {
		t.Fatalf("sector 16 isn't a primary volume descriptor: % x", pvd[:7])
	}
	if term := sector(17); term[0] != 255 || string(term[1:6]) != "CD001" {
		t.Fatalf("sector 17 isn't the descriptor set terminator: % x", term[:7])
	}
	if got, want := binary.LittleEndian.Uint32(pvd[80:84]), uint32(len(img)/SectorSize); got != want { //nolint:gosec // G115: a test image
		t.Errorf("volume space size = %d sectors, image has %d", got, want)
	}
	if lsb, msb := binary.LittleEndian.Uint32(pvd[80:84]), binary.BigEndian.Uint32(pvd[84:88]); lsb != msb {
		t.Errorf("volume space size's two byte orders disagree: %d, %d", lsb, msb)
	}
	if got := binary.LittleEndian.Uint16(pvd[128:130]); got != SectorSize {
		t.Errorf("logical block size = %d", got)
	}

	rb := readBack{volumeID: strings.TrimRight(string(pvd[40:72]), " "), files: map[string][]byte{}}
	root := pvd[156:190]
	rootExtent := binary.LittleEndian.Uint32(root[2:6])
	rootSize := binary.LittleEndian.Uint32(root[10:14])
	if root[25]&0x02 == 0 {
		t.Fatalf("the root record isn't a directory (flags %#x)", root[25])
	}
	if lpt := sector(binary.LittleEndian.Uint32(pvd[140:144])); binary.LittleEndian.Uint32(lpt[2:6]) != rootExtent {
		t.Errorf("the L path table's root extent isn't the root's (%d)", rootExtent)
	}
	if mpt := sector(binary.BigEndian.Uint32(pvd[148:152])); binary.BigEndian.Uint32(mpt[2:6]) != rootExtent {
		t.Errorf("the M path table's root extent isn't the root's (%d)", rootExtent)
	}

	dir := img[int(rootExtent)*SectorSize : int(rootExtent)*SectorSize+int(rootSize)]
	for off := 0; off < len(dir); {
		n := int(dir[off])
		if n == 0 { // the rest of this sector is padding
			off = (off/SectorSize + 1) * SectorSize
			continue
		}
		rec := dir[off : off+n]
		off += n
		id := rec[33 : 33+int(rec[32])]
		if len(id) == 1 && (id[0] == 0 || id[0] == 1) {
			continue // "." and ".."
		}
		if rec[25]&0x02 != 0 {
			t.Errorf("unexpected subdirectory %q", id)
			continue
		}
		extent := binary.LittleEndian.Uint32(rec[2:6])
		size := binary.LittleEndian.Uint32(rec[10:14])
		if binary.BigEndian.Uint32(rec[6:10]) != extent || binary.BigEndian.Uint32(rec[14:18]) != size {
			t.Errorf("record %q: its two byte orders disagree", id)
		}
		rb.order = append(rb.order, string(id))
		name := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(string(id), ";1"), "."))
		rb.files[name] = img[int(extent)*SectorSize : int(extent)*SectorSize+int(size)]
	}
	return rb
}

func seedFiles() []File {
	return []File{
		{Name: "install.yaml", Data: []byte("force_install: false\n")},
		{Name: "network.yaml", Data: []byte("wireguard:\n  - name: wg0\n")},
		{Name: "applications.yaml", Data: []byte("applications:\n  - name: incus\n")},
		{Name: "incus.yaml", Data: []byte("apply_defaults: true\n")},
	}
}

func TestWriteVolumeIDAndRootFiles(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Options{VolumeID: "SEED_DATA"}, seedFiles()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rb := read(t, buf.Bytes())
	if rb.volumeID != "SEED_DATA" {
		t.Errorf("volume ID = %q, want SEED_DATA", rb.volumeID)
	}
	want := seedFiles()
	if len(rb.files) != len(want) {
		t.Errorf("root holds %d files, want %d: %v", len(rb.files), len(want), rb.order)
	}
	for _, f := range want {
		got, ok := rb.files[f.Name]
		if !ok {
			t.Errorf("%s missing from the root (have %v)", f.Name, rb.order)
			continue
		}
		if !bytes.Equal(got, f.Data) {
			t.Errorf("%s = %q, want %q", f.Name, got, f.Data)
		}
	}
	wantOrder := []string{"APPLICATIONS.YAML;1", "INCUS.YAML;1", "INSTALL.YAML;1", "NETWORK.YAML;1"}
	if fmt.Sprint(rb.order) != fmt.Sprint(wantOrder) {
		t.Errorf("directory order = %v, want %v (sorted, as ISO 9660 requires)", rb.order, wantOrder)
	}
}

// TestWriteIsReproducible: with no ModTime, the same files give the same
// bytes, whatever order they're passed in.
func TestWriteIsReproducible(t *testing.T) {
	var a, b bytes.Buffer
	files := seedFiles()
	if err := Write(&a, Options{VolumeID: "SEED_DATA"}, files); err != nil {
		t.Fatal(err)
	}
	files[0], files[3] = files[3], files[0]
	if err := Write(&b, Options{VolumeID: "SEED_DATA"}, files); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("two writes of the same files differ")
	}
}

func TestWriteModTime(t *testing.T) {
	var buf bytes.Buffer
	mod := time.Date(2026, 11, 10, 12, 34, 56, 0, time.UTC)
	if err := Write(&buf, Options{VolumeID: "SEED_DATA", ModTime: mod}, seedFiles()); err != nil {
		t.Fatal(err)
	}
	pvd := buf.Bytes()[16*SectorSize:]
	if got := string(pvd[813:829]); got != "2026111012345600" {
		t.Errorf("creation date = %q", got)
	}
	if got := pvd[156+18 : 156+25]; !bytes.Equal(got, []byte{126, 11, 10, 12, 34, 56, 0}) {
		t.Errorf("root record date = %v", got)
	}
}

// TestWriteSpansSectors covers a file bigger than a sector and a directory
// bigger than one: records don't straddle a sector boundary, and every file
// reads back whole.
func TestWriteSpansSectors(t *testing.T) {
	var files []File
	for i := range 80 {
		files = append(files, File{Name: fmt.Sprintf("file_%02d.yaml", i), Data: bytes.Repeat([]byte{byte(i)}, 100*i)})
	}
	files = append(files, File{Name: "empty", Data: nil})
	var buf bytes.Buffer
	if err := Write(&buf, Options{VolumeID: "X"}, files); err != nil {
		t.Fatal(err)
	}
	rb := read(t, buf.Bytes())
	if len(rb.files) != len(files) {
		t.Fatalf("read back %d files, want %d", len(rb.files), len(files))
	}
	for _, f := range files {
		if got := rb.files[f.Name]; !bytes.Equal(got, f.Data) {
			t.Errorf("%s: read back %d bytes, want %d", f.Name, len(got), len(f.Data))
		}
	}
}

func TestWriteRejectsBadNames(t *testing.T) {
	cases := map[string]struct {
		opts  Options
		files []File
		want  string
	}{
		"dash in name":      {Options{VolumeID: "SEED_DATA"}, []File{{Name: "seed-data.yaml"}}, "only letters, digits and _"},
		"two dots":          {Options{VolumeID: "SEED_DATA"}, []File{{Name: "seed.tar.gz"}}, "only letters, digits and _"},
		"too long":          {Options{VolumeID: "SEED_DATA"}, []File{{Name: strings.Repeat("a", 31)}}, "longer than"},
		"empty name":        {Options{VolumeID: "SEED_DATA"}, []File{{Name: ""}}, "empty file name"},
		"case collision":    {Options{VolumeID: "SEED_DATA"}, []File{{Name: "a.yaml"}, {Name: "A.YAML"}}, "the same on ISO 9660"},
		"volume ID charset": {Options{VolumeID: "SEED-DATA"}, nil, "volume ID"},
		"volume ID length":  {Options{VolumeID: strings.Repeat("A", 33)}, nil, "volume ID is longer"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			err := Write(&buf, tc.opts, tc.files)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Write error = %v, want one containing %q", err, tc.want)
			}
			if buf.Len() != 0 {
				t.Errorf("a refused Write wrote %d bytes", buf.Len())
			}
		})
	}
}

// TestBlkidSeesLabel asks util-linux's own prober, where it's installed,
// what it makes of the image: the filesystem type and label udev files it
// under (/dev/disk/by-label/SEED_DATA is how incus-osd finds it).
func TestBlkidSeesLabel(t *testing.T) {
	blkid, err := exec.LookPath("blkid")
	if err != nil {
		t.Skip("blkid not installed")
	}
	var buf bytes.Buffer
	if err := Write(&buf, Options{VolumeID: "SEED_DATA"}, seedFiles()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "seed.iso")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(blkid, "-p", "-o", "export", path).CombinedOutput() //nolint:gosec // G204: blkid from $PATH on a test file
	if err != nil {
		t.Fatalf("blkid -p: %v: %s", err, out)
	}
	for _, want := range []string{"TYPE=iso9660", "LABEL=SEED_DATA"} {
		if !strings.Contains(string(out), want+"\n") {
			t.Errorf("blkid output lacks %s:\n%s", want, out)
		}
	}
}

// TestWriteOrdersRecordsAsECMA119: records sort by name, then extension,
// each space-padded (9.3), not by the identifiers' raw bytes, where ';'
// sorts after the digits.
func TestWriteOrdersRecordsAsECMA119(t *testing.T) {
	files := []File{
		{Name: "a.b1", Data: []byte("1")},
		{Name: "ab.c", Data: []byte("2")},
		{Name: "a.b", Data: []byte("3")},
		{Name: "a1.b", Data: []byte("4")},
	}
	var buf bytes.Buffer
	if err := Write(&buf, Options{VolumeID: "SEED_DATA"}, files); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := []string{"A.B;1", "A.B1;1", "A1.B;1", "AB.C;1"}
	if got := read(t, buf.Bytes()).order; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("directory order = %v, want %v (ECMA-119 9.3)", got, want)
	}
}
