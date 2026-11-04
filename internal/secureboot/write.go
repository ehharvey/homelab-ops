package secureboot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// File names Write uses. The .auth names are the ones systemd-boot's
// auto-enroll looks for in /loader/keys/<name>/ on the ESP.
const (
	PKCertFile    = "PK.crt"
	KEKCertFile   = "KEK.crt"
	DBCertFile    = "db.crt"
	PKKeyFile     = "PK.key"
	KEKKeyFile    = "KEK.key"
	DBKeyFile     = "db.key"
	PKESLFile     = "PK.esl"
	KEKESLFile    = "KEK.esl"
	DBESLFile     = "db.esl"
	DBXESLFile    = "dbx.esl"
	PKAuthFile    = "PK.auth"
	KEKAuthFile   = "KEK.auth"
	DBAuthFile    = "db.auth"
	DBXAuthFile   = "dbx.auth"
	OwnerGUIDFile = "GUID"
)

// Write persists a hierarchy and its payloads. The PK and KEK private keys
// go to offlineDir, alone, so that directory can be moved to offline
// storage whole (§31: PK and KEK private keys live offline). Everything else
// goes to outputDir: the three certificates (PEM), the db private key the
// signer needs day to day, the signature lists, the signed .auth enrollment
// payloads (dbx's only when p has one), and the owner GUID. Private keys are
// written 0600.
//
// Write refuses to overwrite any existing file unless force is true, and
// checks every target before writing any. It writes through WriteFiles, so a
// failure part-way leaves the previous set as it was. With force and no dbx
// in p, it removes a dbx.esl and dbx.auth an earlier run left in outputDir,
// so systemd-boot does not enroll a stale dbx beside the new keys; without
// force, their presence is refused like any other existing file.
func Write(outputDir, offlineDir string, h *Hierarchy, p *Payloads, force bool) ([]string, error) {
	pkKey, err := h.PK.KeyPEM()
	if err != nil {
		return nil, fmt.Errorf("PK: %w", err)
	}
	kekKey, err := h.KEK.KeyPEM()
	if err != nil {
		return nil, fmt.Errorf("KEK: %w", err)
	}
	dbKey, err := h.DB.KeyPEM()
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}

	out := func(name string) string { return filepath.Join(outputDir, name) }
	off := func(name string) string { return filepath.Join(offlineDir, name) }
	files := []File{
		{Path: off(PKKeyFile), Data: pkKey, Private: true},
		{Path: off(KEKKeyFile), Data: kekKey, Private: true},
		{Path: out(DBKeyFile), Data: dbKey, Private: true},
		{Path: out(PKCertFile), Data: h.PK.CertPEM()},
		{Path: out(KEKCertFile), Data: h.KEK.CertPEM()},
		{Path: out(DBCertFile), Data: h.DB.CertPEM()},
		{Path: out(PKESLFile), Data: p.PKESL},
		{Path: out(KEKESLFile), Data: p.KEKESL},
		{Path: out(DBESLFile), Data: p.DBESL},
		{Path: out(PKAuthFile), Data: p.PKAuth},
		{Path: out(KEKAuthFile), Data: p.KEKAuth},
		{Path: out(DBAuthFile), Data: p.DBAuth},
	}
	var stale []string
	if p.DBXAuth != nil {
		files = append(files,
			File{Path: out(DBXESLFile), Data: p.DBXESL},
			File{Path: out(DBXAuthFile), Data: p.DBXAuth})
	} else {
		stale = []string{out(DBXESLFile), out(DBXAuthFile)}
	}
	files = append(files, File{Path: out(OwnerGUIDFile), Data: []byte(h.OwnerGUID.String() + "\n")})

	if !force {
		for _, path := range stale {
			if _, statErr := os.Lstat(path); statErr == nil {
				return nil, fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}
		}
	}
	for _, dir := range []string{outputDir, offlineDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := WriteFiles(files, force); err != nil {
		return nil, err
	}
	for _, path := range stale {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("remove the earlier run's %s: %w", path, err)
		}
	}

	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return paths, nil
}
