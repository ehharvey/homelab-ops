package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestGenCertCommandWritesFiles(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "cert")

	rootCmd.SetArgs([]string{"gen-cert", "--output-dir", outDir})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	if _, err := os.Stat(filepath.Join(outDir, "client.crt")); err != nil {
		t.Errorf("client.crt not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "client.key")); err != nil {
		t.Errorf("client.key not written: %v", err)
	}
}
