package cmd

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// minimalEFI returns a one-section PE32+ EFI application, enough for
// sign-efi to hash and sign. internal/secureboot's tests cover the layout
// details; this only drives the command.
func minimalEFI(t *testing.T) []byte {
	t.Helper()
	const opt, optSize, hdrs, align = 0x40 + 4 + 20, 112 + 16*8, 0x200, 0x200
	b := make([]byte, hdrs+align)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x40)
	copy(b[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[0x44:], 0x8664)  // Machine
	binary.LittleEndian.PutUint16(b[0x46:], 1)       // NumberOfSections
	binary.LittleEndian.PutUint16(b[0x54:], optSize) // SizeOfOptionalHeader
	binary.LittleEndian.PutUint16(b[opt:], 0x20b)    // PE32+
	binary.LittleEndian.PutUint32(b[opt+32:], 0x1000)
	binary.LittleEndian.PutUint32(b[opt+36:], align)
	binary.LittleEndian.PutUint32(b[opt+56:], 0x2000)
	binary.LittleEndian.PutUint32(b[opt+60:], hdrs)
	binary.LittleEndian.PutUint16(b[opt+68:], 10) // EFI application
	binary.LittleEndian.PutUint32(b[opt+108:], 16)
	sec := opt + optSize
	copy(b[sec:], ".text")
	binary.LittleEndian.PutUint32(b[sec+8:], align)
	binary.LittleEndian.PutUint32(b[sec+12:], 0x1000)
	binary.LittleEndian.PutUint32(b[sec+16:], align)
	binary.LittleEndian.PutUint32(b[sec+20:], hdrs)
	for i := range align {
		b[hdrs+i] = byte(i)
	}
	if _, err := pe.NewFile(bytes.NewReader(b)); err != nil {
		t.Fatalf("minimal EFI does not parse: %v", err)
	}
	return b
}

func TestSignEFISignsWithTheDBKey(t *testing.T) {
	dir := t.TempDir()
	sbDir := filepath.Join(dir, "sb")
	if out, err := runBootstrap(t, "gen-secureboot-keys", "--output-dir", sbDir, "--offline-dir", filepath.Join(dir, "offline")); err != nil {
		t.Fatalf("gen-secureboot-keys: %v\n%s", err, out)
	}
	input := filepath.Join(dir, "uki.efi")
	if err := os.WriteFile(input, minimalEFI(t), 0o600); err != nil {
		t.Fatal(err)
	}
	keyArgs := []string{"--key", filepath.Join(sbDir, "db.key"), "--cert", filepath.Join(sbDir, "db.crt")}

	out, err := runBootstrap(t, append([]string{"sign-efi", input}, keyArgs...)...)
	if err != nil {
		t.Fatalf("sign-efi: %v\n%s", err, out)
	}
	signed, err := os.ReadFile(input + ".signed")
	if err != nil {
		t.Fatalf("default output not written: %v", err)
	}
	f, err := pe.NewFile(bytes.NewReader(signed))
	if err != nil {
		t.Fatalf("signed image does not parse: %v", err)
	}
	dirs := f.OptionalHeader.(*pe.OptionalHeader64).DataDirectory
	if sec := dirs[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]; sec.VirtualAddress == 0 || sec.Size == 0 {
		t.Error("signed image has no Certificate Table")
	}

	if _, err := runBootstrap(t, append([]string{"sign-efi", input}, keyArgs...)...); err == nil {
		t.Error("existing output without --force: want an error")
	}
	if out, err := runBootstrap(t, append([]string{"sign-efi", input, "--force"}, keyArgs...)...); err != nil {
		t.Errorf("existing output with --force: %v\n%s", err, out)
	}
	if _, err := runBootstrap(t, append([]string{"sign-efi", input + ".signed", "--output", filepath.Join(dir, "twice.efi")}, keyArgs...)...); err == nil {
		t.Error("signing a signed image: want an error")
	}
	if _, err := runBootstrap(t, append([]string{"sign-efi", input, "--output", filepath.Join(dir, "kek.efi")},
		"--key", filepath.Join(sbDir, "db.key"), "--cert", filepath.Join(sbDir, "KEK.crt"))...); err == nil {
		t.Error("key and cert that don't match: want an error")
	}
}
