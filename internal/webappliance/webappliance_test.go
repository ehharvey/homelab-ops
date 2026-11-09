package webappliance

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"github.com/ehharvey/homelab-ops/deploy"
)

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// configDir writes a deployment config directory like examples/webapp/,
// filled in: web.env, operator-peers, client.crt and secrets/wireguard.key.
func configDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel string, mode os.FileMode, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("web.env", 0o644, "WIREGUARD_ENDPOINT=webapp.example.net:51820\nCONFIG_REPO_URL=https://example.invalid/fleet.git\n")
	write("operator-peers", 0o644, "10.100.0.240=b3BlcmF0b3Itc2VlZC1wdWJsaWMta2V5LXRlc3QxMjM=\n")
	write("client.crt", 0o644, "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	if err := os.Mkdir(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	write("secrets/wireguard.key", 0o600, "c2VjcmV0LXdpcmVndWFyZC1rZXktZm9yLXRlc3RzMDA=\n")
	return dir
}

// parse splits user-data into its cloud-config, keyed by write_files path.
func parse(t *testing.T, s Seed) (cloudConfig, map[string]writeFile) {
	t.Helper()
	if !bytes.HasPrefix(s.UserData, []byte("#cloud-config\n")) {
		t.Fatalf("user-data doesn't start with #cloud-config:\n%s", s.UserData)
	}
	var cc cloudConfig
	if err := yaml.Unmarshal(s.UserData, &cc); err != nil {
		t.Fatalf("user-data isn't YAML: %v\n%s", err, s.UserData)
	}
	files := map[string]writeFile{}
	for _, f := range cc.WriteFiles {
		if _, dup := files[f.Path]; dup {
			t.Fatalf("write_files has %s twice", f.Path)
		}
		files[f.Path] = f
	}
	return cc, files
}

func content(t *testing.T, f writeFile) string {
	t.Helper()
	if f.Encoding == "b64" {
		b, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return f.Content
}

func settingsOf(t *testing.T, files map[string]writeFile) Settings {
	t.Helper()
	f, ok := files[SettingsPath]
	if !ok {
		t.Fatalf("no %s in write_files", SettingsPath)
	}
	var s Settings
	if err := json.Unmarshal([]byte(content(t, f)), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func render(t *testing.T, o Options) Seed {
	t.Helper()
	s, err := Render(o)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return s
}

func TestRenderCarriesConfigFilesInlineWithModes(t *testing.T) {
	dir := configDir(t)
	s := render(t, Options{ConfigDir: dir})
	cc, files := parse(t, s)

	for rel, mode := range map[string]string{
		"web.env":               "0644",
		"operator-peers":        "0644",
		"client.crt":            "0644",
		"secrets/wireguard.key": "0600",
	} {
		f, ok := files[ConfigDir+"/"+rel]
		if !ok {
			t.Errorf("no %s in write_files", rel)
			continue
		}
		want, _ := os.ReadFile(filepath.Join(dir, rel))
		if got := content(t, f); got != string(want) {
			t.Errorf("%s: content %q, want %q", rel, got, want)
		}
		if f.Permissions != mode {
			t.Errorf("%s: permissions %s, want %s", rel, f.Permissions, mode)
		}
		if f.Owner != "root:root" {
			t.Errorf("%s: owner %s", rel, f.Owner)
		}
	}

	// secrets/ is created 0700 before write_files runs, rather than 0755 as
	// cloud-init makes a missing parent.
	want := []string{"cloud-init-per", "once", "", "install", "-d", "-m", "0700", ConfigDir + "/secrets"}
	found := false
	for _, c := range cc.Bootcmd {
		if len(c) == len(want) && c[len(c)-1] == want[len(want)-1] {
			found = true
			for i := range want {
				if i != 2 && c[i] != want[i] {
					t.Errorf("bootcmd for secrets/: %q, want %q", c, want)
					break
				}
			}
		}
	}
	if !found {
		t.Errorf("no bootcmd creates %s/secrets 0700: %q", ConfigDir, cc.Bootcmd)
	}
	// cloud-init's YAML 1.1 reads a bare 0700 as the integer 448, so modes
	// are quoted strings.
	if !bytes.Contains(s.UserData, []byte(`"0700", `+ConfigDir+"/secrets]")) ||
		!bytes.Contains(s.UserData, []byte(`permissions: "0600"`)) {
		t.Errorf("modes aren't quoted strings:\n%s", s.UserData)
	}
	// First, once per seed, empty the config directory, so a re-seed
	// replaces it whole; then create the directory itself.
	wantClear := command{"cloud-init-per", "once", "homelab-ops-clear-web",
		"find", EtcDir, "-mindepth", "2", "-path", ConfigDir + "/*", "-delete"}
	if got := cc.Bootcmd[0]; strings.Join(got, " ") != strings.Join(wantClear, " ") {
		t.Errorf("first bootcmd %q, want %q", got, wantClear)
	}
	if got := cc.Bootcmd[1]; got[len(got)-1] != ConfigDir {
		t.Errorf("second bootcmd should create %s itself, got %q", ConfigDir, got)
	}
}

// The clear bootcmd's find, run on a real tree: it empties web/, hidden
// entries and nested directories included, and spares web/ itself and the
// rest of the tree.
func TestClearBootcmdEmptiesOnlyTheConfigDir(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc", "homelab-ops")
	for _, rel := range []string{"web/secrets/wireguard.key", "web/.hidden", "web/operator-peers", "appliance/settings.json", "compose.yaml"} {
		p := filepath.Join(etc, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cc, _ := parse(t, render(t, Options{ConfigDir: configDir(t)}))
	argv := cc.Bootcmd[0][3:] // less cloud-init-per once <name>
	for i, a := range argv {
		argv[i] = strings.Replace(a, EtcDir, etc, 1)
	}
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil { //nolint:gosec // G204: the seed's own argv, under a temp dir
		t.Fatalf("%q: %v\n%s", argv, err, out)
	}
	left, err := os.ReadDir(filepath.Join(etc, "web"))
	if err != nil {
		t.Fatalf("web/ itself should stay: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("web/ still holds %v", left)
	}
	for _, rel := range []string{"appliance/settings.json", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(etc, rel)); err != nil {
			t.Errorf("%s should stay: %v", rel, err)
		}
	}
}

func TestRenderCarriesBinaryFilesAsBase64(t *testing.T) {
	dir := configDir(t)
	bin := []byte{0, 1, 2, 0xff, 0xfe}
	if err := os.WriteFile(filepath.Join(dir, "secrets", "blob"), bin, 0o600); err != nil {
		t.Fatal(err)
	}
	_, files := parse(t, render(t, Options{ConfigDir: dir}))
	f := files[ConfigDir+"/secrets/blob"]
	if f.Encoding != "b64" || content(t, f) != string(bin) {
		t.Errorf("binary file: encoding %q, content %q", f.Encoding, content(t, f))
	}
}

func TestRenderNoSecretsLeavesOutExactlySecrets(t *testing.T) {
	dir := configDir(t)
	keys := []string{testKey(t, "op@laptop")}
	s := render(t, Options{ConfigDir: dir, NoSecrets: true, SSHAuthorizedKeys: keys})
	cc, files := parse(t, s)

	for p := range files {
		if strings.HasPrefix(p, ConfigDir+"/secrets") {
			t.Errorf("--no-secrets still carries %s", p)
		}
	}
	for _, c := range cc.Bootcmd {
		if strings.HasPrefix(c[len(c)-1], ConfigDir+"/secrets") {
			t.Errorf("--no-secrets still creates %s", c[len(c)-1])
		}
	}
	if bytes.Contains(s.UserData, []byte("c2VjcmV0LXdpcmVndWFyZC1rZXk")) {
		t.Error("--no-secrets user-data contains the key")
	}
	for _, rel := range []string{"web.env", "operator-peers", "client.crt"} {
		if _, ok := files[ConfigDir+"/"+rel]; !ok {
			t.Errorf("--no-secrets left out %s too", rel)
		}
	}

	// Without --no-secrets, a directory with no secrets/ is refused: the
	// appliance would wait for it forever.
	if err := os.RemoveAll(filepath.Join(dir, "secrets")); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(Options{ConfigDir: dir}); err == nil || !strings.Contains(err.Error(), "--no-secrets") {
		t.Errorf("no secrets/, no --no-secrets: err %v, want a refusal pointing at --no-secrets", err)
	}
	render(t, Options{ConfigDir: dir, NoSecrets: true, SSHAuthorizedKeys: keys})

	// --no-secrets without SSH keys leaves no way to deliver secrets/.
	if _, err := Render(Options{ConfigDir: dir, NoSecrets: true}); err == nil || !strings.Contains(err.Error(), "--ssh-authorized-keys") {
		t.Errorf("--no-secrets without SSH keys: err %v, want a refusal pointing at --ssh-authorized-keys", err)
	}
}

func TestRenderPinsImageDigestInComposeFile(t *testing.T) {
	pinned, err := ComposeImage(deploy.ComposeYAML)
	if err != nil {
		t.Fatal(err)
	}
	if !imageRe.MatchString(pinned) {
		t.Fatalf("deploy/compose.yaml's image %q isn't pinned by digest", pinned)
	}
	repo, _, _ := strings.Cut(pinned, "@")

	for _, tc := range []struct{ image, want string }{
		{"", pinned},
		{testDigest, repo + "@" + testDigest},
		{"registry.example:5000/web@" + testDigest, "registry.example:5000/web@" + testDigest},
	} {
		_, files := parse(t, render(t, Options{ConfigDir: configDir(t), Image: tc.image}))
		f, ok := files[ComposePath]
		if !ok {
			t.Fatalf("no %s in write_files", ComposePath)
		}
		got := content(t, f)
		img, err := ComposeImage([]byte(got))
		if err != nil {
			t.Fatal(err)
		}
		if img != tc.want {
			t.Errorf("--image %q: compose image %q, want %q", tc.image, img, tc.want)
		}
		// Only the image line changes, and the "This pin is" note above
		// it goes with a new pin: other comments and layout survive.
		var wantLines []string
		for _, l := range strings.Split(string(deploy.ComposeYAML), "\n") {
			switch {
			case tc.image != "" && strings.HasPrefix(strings.TrimSpace(l), pinNote):
				continue
			case strings.Contains(l, "image: "+pinned):
				l = strings.Replace(l, pinned, tc.want, 1)
			}
			wantLines = append(wantLines, l)
		}
		if want := strings.Join(wantLines, "\n"); got != want {
			t.Errorf("--image %q: compose.yaml\n%s\nwant\n%s", tc.image, got, want)
		}
		if tc.image == "" && !strings.Contains(got, pinNote) {
			t.Errorf("the default pin lost its %q note", pinNote)
		}
		if tc.image != "" && strings.Contains(got, pinNote) {
			t.Errorf("--image %q: compose.yaml keeps the old pin's %q note", tc.image, pinNote)
		}
		if f.Permissions != "0644" {
			t.Errorf("compose.yaml permissions %s", f.Permissions)
		}
	}

	// The committed pin's note is the line directly above image:, where
	// PinImage looks for it.
	if !strings.Contains(string(deploy.ComposeYAML), pinNote+"ghcr.io/") ||
		!regexp.MustCompile(regexp.QuoteMeta(pinNote)+`[^\n]*\n\s*image: `).Match(deploy.ComposeYAML) {
		t.Errorf("deploy/compose.yaml: no %q line directly above image:", pinNote)
	}

	for _, bad := range []string{
		"ghcr.io/ehharvey/homelab-ops/web:latest",
		"ghcr.io/ehharvey/homelab-ops/web",
		"sha256:abc",
		"ghcr.io/x/web@sha256:" + strings.Repeat("A", 64),
	} {
		if _, err := Render(Options{ConfigDir: configDir(t), Image: bad}); err == nil {
			t.Errorf("--image %q accepted; want a refusal (digest required)", bad)
		}
	}
}

// A bare digest replaces the whole pin: a tag the old reference carried
// named the old digest, so it goes too.
func TestPinImageBareDigestDropsTag(t *testing.T) {
	old := "sha256:" + strings.Repeat("0", 64)
	for _, tc := range []struct{ cur, want string }{
		{"ghcr.io/x/web:v1@" + old, "ghcr.io/x/web@" + testDigest},
		{"registry.example:5000/web:v1@" + old, "registry.example:5000/web@" + testDigest},
		{"registry.example:5000/web@" + old, "registry.example:5000/web@" + testDigest},
	} {
		compose := []byte("services:\n  web:\n    # This pin is the old one.\n    image: " + tc.cur + "\n")
		got, err := PinImage(compose, testDigest)
		if err != nil {
			t.Fatal(err)
		}
		if want := "services:\n  web:\n    image: " + tc.want + "\n"; string(got) != want {
			t.Errorf("%s: got\n%s\nwant\n%s", tc.cur, got, want)
		}
	}
	// The same reference again changes nothing, the note included.
	compose := []byte("services:\n  web:\n    # This pin is it.\n    image: ghcr.io/x/web@" + testDigest + "\n")
	if got, err := PinImage(compose, testDigest); err != nil || !bytes.Equal(got, compose) {
		t.Errorf("re-pinning the same digest: %q, %v; want it unchanged", got, err)
	}
}

func TestRenderRebootTime(t *testing.T) {
	_, files := parse(t, render(t, Options{ConfigDir: configDir(t)}))
	if got := settingsOf(t, files).RebootTime; got != DefaultRebootTime {
		t.Errorf("default reboot time %q, want %q", got, DefaultRebootTime)
	}
	_, files = parse(t, render(t, Options{ConfigDir: configDir(t), RebootTime: "23:15"}))
	if got := settingsOf(t, files).RebootTime; got != "23:15" {
		t.Errorf("reboot time %q, want 23:15", got)
	}
	if f := files[SettingsPath]; f.Permissions != "0644" {
		t.Errorf("settings.json permissions %s", f.Permissions)
	}
	for _, bad := range []string{"4:00", "24:00", "04:60", "0400", "noon"} {
		if _, err := Render(Options{ConfigDir: configDir(t), RebootTime: bad}); err == nil {
			t.Errorf("reboot time %q accepted", bad)
		}
	}
}

func testKey(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment
}

func TestRenderSSHKeysOnlyWhenGiven(t *testing.T) {
	s := render(t, Options{ConfigDir: configDir(t)})
	_, files := parse(t, s)
	if keys := settingsOf(t, files).SSHAuthorizedKeys; keys == nil || len(keys) != 0 {
		t.Errorf("no keys given: sshAuthorizedKeys %#v, want []", keys)
	}
	if bytes.Contains(s.UserData, []byte("ssh-ed25519")) {
		t.Error("no keys given, but user-data holds one")
	}
	// settings.json's [] is what appliance.nix compares against.
	if c := content(t, files[SettingsPath]); !strings.Contains(c, `"sshAuthorizedKeys":[]`) {
		t.Errorf("settings.json %s, want sshAuthorizedKeys []", c)
	}

	k1, k2 := testKey(t, "op@laptop"), testKey(t, "op@phone")
	_, files = parse(t, render(t, Options{ConfigDir: configDir(t), SSHAuthorizedKeys: []string{k1, k2}}))
	if keys := settingsOf(t, files).SSHAuthorizedKeys; len(keys) != 2 || keys[0] != k1 || keys[1] != k2 {
		t.Errorf("sshAuthorizedKeys %q, want %q", keys, []string{k1, k2})
	}

	// Render, the exported API, holds keys to ParseAuthorizedKeys' rules.
	for name, bad := range map[string]string{
		"invalid":        "ssh-ed25519 notbase64",
		"two lines":      k1 + "\n" + k2,
		"a trailing CR":  k1 + "\r",
		"options":        `command="/bin/sh" ` + k1,
		"from= option":   `from="10.0.0.1" ` + k1,
		"empty":          "",
		"only a comment": "# op@laptop",
	} {
		if _, err := Render(Options{ConfigDir: configDir(t), SSHAuthorizedKeys: []string{bad}}); err == nil {
			t.Errorf("%s: key %q accepted", name, bad)
		}
	}
}

func TestParseAuthorizedKeys(t *testing.T) {
	k1, k2 := testKey(t, "a"), testKey(t, "b")
	keys, err := ParseAuthorizedKeys([]byte("# operators\n" + k1 + "\n\n  " + k2 + "  \n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != k1 || keys[1] != k2 {
		t.Errorf("keys %q", keys)
	}
	for _, bad := range []string{"", "# only a comment\n", "not a key\n", `from="10.0.0.1" ` + k1 + "\n"} {
		if _, err := ParseAuthorizedKeys([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRenderRefusesWhatTheApplianceCouldNotStartFrom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"secrets/ file readable beyond owner", func(t *testing.T, dir string) {
			mustChmod(t, filepath.Join(dir, "secrets", "wireguard.key"), 0o644)
		}, "secrets/wireguard.key: mode 0644"},
		{"secrets/ dir readable beyond owner", func(t *testing.T, dir string) {
			mustChmod(t, filepath.Join(dir, "secrets"), 0o755)
		}, "secrets: mode 0755"},
		{"WIREGUARD_PORT moved", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "web.env"), []byte("WIREGUARD_ENDPOINT=webapp.example.net:51820\nWIREGUARD_PORT=51821\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "WIREGUARD_PORT=51821"},
		{"a key deployconfig doesn't know", func(t *testing.T, dir string) {
			appendFile(t, filepath.Join(dir, "web.env"), "CONFIG_REPO_UR=https://example.invalid/x.git\n")
		}, "unknown setting CONFIG_REPO_UR"},
		{"a File setting in web.env", func(t *testing.T, dir string) {
			appendFile(t, filepath.Join(dir, "web.env"), "CLIENT_CERT_PATH=/x\n")
		}, "CLIENT_CERT_PATH is set by the directory's client.crt file"},
		{"no WIREGUARD_ENDPOINT", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "operator-peers")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "web.env"), []byte("CONFIG_REPO_URL=https://example.invalid/fleet.git\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "WIREGUARD_ENDPOINT: unset"},
		{"a symlink", func(t *testing.T, dir string) {
			if err := os.Symlink("web.env", filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
		}, "link: not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := configDir(t)
			tc.setup(t, dir)
			_, err := Render(Options{ConfigDir: dir})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want it to name %q", err, tc.want)
			}
		})
	}

	// WIREGUARD_PORT at the published port is fine.
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "web.env"), []byte("WIREGUARD_ENDPOINT=webapp.example.net:51820\nWIREGUARD_PORT=51820\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	render(t, Options{ConfigDir: dir})
	if _, err := Render(Options{ConfigDir: filepath.Join(dir, "missing")}); err == nil {
		t.Error("a missing config dir was accepted")
	}
}

// Render checks the config directory as the seed ships it: path settings
// must name a file it carries under ConfigDir, and files it leaves out are
// neither checked nor counted on.
func TestRenderChecksWhatTheSeedShips(t *testing.T) {
	write := func(t *testing.T, p string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(p, []byte("x\n"), mode); err != nil {
			t.Fatal(err)
		}
		mustChmod(t, p, mode)
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		o     Options
		want  string // "" accepts
	}{
		{"NETWORK_ADDRESSING_PATH in the config dir", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "network.yaml"), 0o644)
			appendFile(t, filepath.Join(dir, "web.env"), "NETWORK_ADDRESSING_PATH="+ConfigDir+"/network.yaml\n")
		}, Options{}, ""},
		{"NETWORK_ADDRESSING_PATH outside the config dir", func(t *testing.T, dir string) {
			appendFile(t, filepath.Join(dir, "web.env"), "NETWORK_ADDRESSING_PATH=/srv/homelab/network.yaml\n")
		}, Options{}, "NETWORK_ADDRESSING_PATH=/srv/homelab/network.yaml: not under " + ConfigDir},
		{"NETWORK_ADDRESSING_PATH escaping the config dir", func(t *testing.T, dir string) {
			appendFile(t, filepath.Join(dir, "web.env"), "NETWORK_ADDRESSING_PATH="+ConfigDir+"/../compose.yaml\n")
		}, Options{}, "not under " + ConfigDir},
		{"BASE_IMAGE_PATH relative", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "base.img"), 0o644)
			appendFile(t, filepath.Join(dir, "web.env"), "BASE_IMAGE_PATH=base.img\n")
		}, Options{}, "BASE_IMAGE_PATH=base.img: not under " + ConfigDir},
		{"BASE_IMAGE_PATH naming a file the config dir lacks", func(t *testing.T, dir string) {
			appendFile(t, filepath.Join(dir, "web.env"), "BASE_IMAGE_PATH="+ConfigDir+"/base.img\n")
		}, Options{}, "the seed carries no file base.img"},
		{"NETWORK_ADDRESSING_PATH naming a dot file the seed skips", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, ".network.yaml"), 0o644)
			appendFile(t, filepath.Join(dir, "web.env"), "NETWORK_ADDRESSING_PATH="+ConfigDir+"/.network.yaml\n")
		}, Options{}, ".network.yaml is left out of the seed"},
		{"NETWORK_ADDRESSING_PATH naming a backup the seed skips", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "network.yaml~"), 0o644)
			appendFile(t, filepath.Join(dir, "web.env"), "NETWORK_ADDRESSING_PATH="+ConfigDir+"/network.yaml~\n")
		}, Options{}, "network.yaml~ is left out of the seed"},
		{"a path under secrets/ with --no-secrets arrives with secrets/", func(t *testing.T, dir string) {
			appendFile(t, filepath.Join(dir, "web.env"), "BASE_IMAGE_PATH="+ConfigDir+"/secrets/base.img\n")
		}, Options{NoSecrets: true}, ""},
		{"secrets/ clutter readable beyond its owner", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "secrets", ".wireguard.key.swp"), 0o644)
			write(t, filepath.Join(dir, "secrets", "wireguard.key~"), 0o644)
		}, Options{}, ""},
		{"--no-secrets with a local secrets/ readable beyond its owner", func(t *testing.T, dir string) {
			mustChmod(t, filepath.Join(dir, "secrets", "wireguard.key"), 0o644)
			mustChmod(t, filepath.Join(dir, "secrets"), 0o755)
		}, Options{NoSecrets: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := configDir(t)
			tc.setup(t, dir)
			o := tc.o
			o.ConfigDir = dir
			if o.NoSecrets {
				o.SSHAuthorizedKeys = []string{testKey(t, "op")}
			}
			_, err := Render(o)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A --config-dir that is a symlink to the directory is the directory.
func TestRenderFollowsSymlinkedConfigDir(t *testing.T) {
	dir := configDir(t)
	link := filepath.Join(t.TempDir(), "web")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	_, files := parse(t, render(t, Options{ConfigDir: link}))
	for _, rel := range []string{"web.env", "secrets/wireguard.key"} {
		if _, ok := files[ConfigDir+"/"+rel]; !ok {
			t.Errorf("through a symlinked --config-dir, %s is missing", rel)
		}
	}
}

// Dot entries (.git/, .DS_Store) and editor backups stay out of user-data,
// and Render names them.
func TestRenderSkipsClutter(t *testing.T) {
	dir := configDir(t)
	for _, d := range []string{".git/objects", "secrets/.cache"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".git/objects/ab", ".git/config", ".DS_Store", "web.env~", "secrets/.cache/x"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("CLUTTER-MARKER\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := render(t, Options{ConfigDir: dir})
	if bytes.Contains(s.UserData, []byte("CLUTTER-MARKER")) || bytes.Contains(s.UserData, []byte("/.git")) {
		t.Errorf("user-data carries clutter:\n%s", s.UserData)
	}
	want := []string{".DS_Store", ".git", "secrets/.cache", "web.env~"}
	if strings.Join(s.Skipped, " ") != strings.Join(want, " ") {
		t.Errorf("Skipped %q, want %q", s.Skipped, want)
	}
	if _, files := parse(t, s); files[ConfigDir+"/web.env"].Path == "" {
		t.Error("web.env left out too")
	}
}

func mustChmod(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestRenderMetaData(t *testing.T) {
	dir := configDir(t)
	a := render(t, Options{ConfigDir: dir})
	b := render(t, Options{ConfigDir: dir, RebootTime: "01:00"})
	var ma, mb map[string]string
	if err := yaml.Unmarshal(a.MetaData, &ma); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b.MetaData, &mb); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ma["instance-id"], "iid-homelab-ops-appliance-") || ma["local-hostname"] != Hostname {
		t.Errorf("meta-data %v", ma)
	}
	if ma["instance-id"] == mb["instance-id"] {
		t.Error("a different seed kept the same instance-id")
	}
	if c := render(t, Options{ConfigDir: dir}); string(c.MetaData) != string(a.MetaData) {
		t.Error("the same inputs gave a different instance-id")
	}
}

func TestWrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "seed")
	s := Seed{UserData: []byte("#cloud-config\n"), MetaData: []byte("instance-id: x\n")}
	if err := Write(out, s, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"user-data", "meta-data"} {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %04o, want 0600", name, info.Mode().Perm())
		}
	}
	if err := Write(out, s, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("overwrite without force: %v", err)
	}
	if err := Write(out, s, true); err != nil {
		t.Errorf("overwrite with force: %v", err)
	}
}

// With no flags, settings.json is deploy/nixos/default-settings.json, which
// the flake's settings-defaults check holds the generic image's settings to:
// a default seed then matches the image and rebuilds nothing at first boot.
func TestRenderDefaultSettingsMatchTheImage(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "deploy", "nixos", "default-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, files := parse(t, render(t, Options{ConfigDir: configDir(t)}))
	if got := content(t, files[SettingsPath]); got != string(want) {
		t.Errorf("default settings.json %q, want default-settings.json's %q", got, want)
	}
}
