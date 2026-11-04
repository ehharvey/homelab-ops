package cmd

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runBootstrap runs the root command with args, resetting the flags these
// tests set so one test's values don't leak into the next.
func runBootstrap(t *testing.T, args ...string) (string, error) {
	t.Helper()
	genSBNoMicrosoftCA, genSBForce, genSBValidityDays = false, false, 3650
	signEFIOutput, signEFIForce = "", false
	rootCmd.SetArgs(args)
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	err := rootCmd.Execute()
	return out.String(), err
}

// eslCount counts the EFI_SIGNATURE_LISTs in a variable value.
func eslCount(t *testing.T, b []byte) int {
	t.Helper()
	n := 0
	for len(b) >= 28 {
		size := int(binary.LittleEndian.Uint32(b[16:]))
		if size < 28 || size > len(b) {
			t.Fatalf("bad SignatureListSize %d", size)
		}
		b = b[size:]
		n++
	}
	if len(b) != 0 {
		t.Fatalf("%d trailing bytes", len(b))
	}
	return n
}

func TestGenSecureBootKeysWritesHierarchyAndPayloads(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extraArgs []string
		dbLists   int
		dbx       bool
	}{
		{"default includes Microsoft UEFI CA and its dbx", nil, 3, true},
		{"--no-microsoft-ca", []string{"--no-microsoft-ca"}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			outDir, offDir := filepath.Join(dir, "sb"), filepath.Join(dir, "offline")
			args := append([]string{"gen-secureboot-keys", "--output-dir", outDir, "--offline-dir", offDir}, tc.extraArgs...)
			out, err := runBootstrap(t, args...)
			if err != nil {
				t.Fatalf("Execute: %v\noutput: %s", err, out)
			}
			for _, f := range []string{"PK.crt", "KEK.crt", "db.crt", "db.key", "PK.esl", "KEK.esl", "db.esl", "PK.auth", "KEK.auth", "db.auth", "GUID"} {
				if _, err := os.Stat(filepath.Join(outDir, f)); err != nil {
					t.Errorf("%s not written: %v", f, err)
				}
			}
			for _, f := range []string{"dbx.esl", "dbx.auth"} {
				if _, err := os.Stat(filepath.Join(outDir, f)); (err == nil) != tc.dbx {
					t.Errorf("%s written: %v, want %v", f, err == nil, tc.dbx)
				}
			}
			for _, f := range []string{"PK.key", "KEK.key"} {
				if _, err := os.Stat(filepath.Join(offDir, f)); err != nil {
					t.Errorf("%s not written to the offline dir: %v", f, err)
				}
				if _, err := os.Stat(filepath.Join(outDir, f)); err == nil {
					t.Errorf("%s written to the output dir", f)
				}
			}
			esl, err := os.ReadFile(filepath.Join(outDir, "db.esl"))
			if err != nil {
				t.Fatal(err)
			}
			if got := eslCount(t, esl); got != tc.dbLists {
				t.Errorf("db.esl has %d signature lists, want %d", got, tc.dbLists)
			}
			if !strings.Contains(out, offDir) {
				t.Errorf("output does not name the offline dir:\n%s", out)
			}

			if _, err := runBootstrap(t, append(args, "--validity-days", "1")...); err == nil {
				t.Error("second run without --force: want an error")
			}
			if out, err := runBootstrap(t, append(args, "--force")...); err != nil {
				t.Errorf("second run with --force: %v\n%s", err, out)
			}
		})
	}
}
