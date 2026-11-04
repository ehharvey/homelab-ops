package deployconfig

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// key is a valid-looking WireGuard public key: 32 bytes of b, base64.
func key(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

// clearEnv unsets every contract setting for the test's duration. t.Setenv
// to "" alone would count as set: an empty env var overrides the directory.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, s := range Settings {
		t.Setenv(s.Name, "") // registers the restore
		if err := os.Unsetenv(s.Name); err != nil {
			t.Fatal(err)
		}
	}
}

// configDir writes a deployment config directory and points
// DEPLOYMENT_CONFIG_DIR at it. files maps a relative path to its contents.
func configDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DEPLOYMENT_CONFIG_DIR", dir)
	return dir
}

func mustLoad(t *testing.T) Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg := mustLoad(t)
	if cfg.Dir != "" || cfg.HealthListenAddr != DefaultHealthListenAddr || cfg.WireGuardPort != DefaultWireGuardPort ||
		cfg.StorePath != DefaultStorePath || cfg.SyncInterval != 0 || cfg.WireGuardEndpoint != "" ||
		cfg.APIHostListenAddr != "" || cfg.ClientCertPath != "" || len(cfg.OperatorPeers) != 0 ||
		cfg.ConfigRepoPrivate || cfg.NetworkAddressingPath != "" {
		t.Errorf("Load with nothing set = %+v, want only the defaults", cfg)
	}
}

// The precedence rule (#241): the environment over the directory, and an
// env var set to "" still overrides it, meaning the setting's default.
func TestLoadPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		file    string // CONFIG_REPO_REF in web.env; "" leaves it out
		env     *string
		wantRef string
		wantInt time.Duration
	}{
		{name: "file only", file: "release", wantRef: "release"},
		{name: "env only", env: ptr("dev"), wantRef: "dev"},
		{name: "both: env wins", file: "release", env: ptr("dev"), wantRef: "dev"},
		{name: "env set empty overrides the file", file: "release", env: ptr(""), wantRef: ""},
		{name: "neither", wantRef: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			webEnv := "CONFIG_REPO_URL=git://example.net/fleet.git\n"
			if tc.file != "" {
				webEnv += "CONFIG_REPO_REF=" + tc.file + "\n"
			}
			configDir(t, map[string]string{WebEnvFile: webEnv})
			if tc.env != nil {
				t.Setenv("CONFIG_REPO_REF", *tc.env)
			}
			cfg := mustLoad(t)
			if cfg.ConfigRepoRef != tc.wantRef {
				t.Errorf("ConfigRepoRef = %q, want %q", cfg.ConfigRepoRef, tc.wantRef)
			}
			if cfg.ConfigRepoURL != "git://example.net/fleet.git" {
				t.Errorf("ConfigRepoURL = %q, want web.env's", cfg.ConfigRepoURL)
			}
		})
	}
}

// An empty env var means the setting's default, not "", for a setting that
// has one, even when web.env sets it.
func TestLoadEnvEmptyMeansDefault(t *testing.T) {
	clearEnv(t)
	configDir(t, map[string]string{WebEnvFile: "HEALTH_LISTEN_ADDR=:9090\nCONFIG_SYNC_INTERVAL=5m\n"})
	t.Setenv("HEALTH_LISTEN_ADDR", "")
	t.Setenv("CONFIG_SYNC_INTERVAL", "")
	cfg := mustLoad(t)
	if cfg.HealthListenAddr != DefaultHealthListenAddr {
		t.Errorf("HealthListenAddr = %q, want the default %q", cfg.HealthListenAddr, DefaultHealthListenAddr)
	}
	if cfg.SyncInterval != 0 {
		t.Errorf("SyncInterval = %v, want 0 (off)", cfg.SyncInterval)
	}
}

// The directory's own files follow the same rule: client.crt and
// operator-peers, overridden by their env vars.
func TestLoadFileSettings(t *testing.T) {
	peersFile := "# operator devices\n10.100.0.240=" + key('a') + "  # laptop\n\n10.100.0.241=" + key('b') + "\n"
	cases := []struct {
		name      string
		files     bool
		certEnv   *string
		peersEnv  *string
		wantCert  string // "dir" means the directory's client.crt
		wantPeers int
	}{
		{name: "file only", files: true, wantCert: "dir", wantPeers: 2},
		{name: "env only", certEnv: ptr("ENVCERT"), peersEnv: ptr("10.100.0.242=" + key('c')), wantCert: "ENVCERT", wantPeers: 1},
		{name: "both: env wins", files: true, certEnv: ptr("ENVCERT"), peersEnv: ptr("10.100.0.242=" + key('c')), wantCert: "ENVCERT", wantPeers: 1},
		{name: "env set empty overrides the files", files: true, certEnv: ptr(""), peersEnv: ptr(""), wantCert: "", wantPeers: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			files := map[string]string{WebEnvFile: "WIREGUARD_ENDPOINT=webapp.example.net:51820\n"}
			if tc.files {
				files[ClientCertFile] = "cert"
				files[OperatorPeersFile] = peersFile
			}
			dir := configDir(t, files)
			envCert := filepath.Join(t.TempDir(), "env.crt")
			if err := os.WriteFile(envCert, []byte("cert"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.certEnv != nil {
				v := *tc.certEnv
				if v == "ENVCERT" {
					v = envCert
				}
				t.Setenv("CLIENT_CERT_PATH", v)
			}
			if tc.peersEnv != nil {
				t.Setenv("WIREGUARD_OPERATOR_PEERS", *tc.peersEnv)
			}
			cfg := mustLoad(t)
			want := map[string]string{"dir": filepath.Join(dir, ClientCertFile), "ENVCERT": envCert, "": ""}[tc.wantCert]
			if cfg.ClientCertPath != want {
				t.Errorf("ClientCertPath = %q, want %q", cfg.ClientCertPath, want)
			}
			if len(cfg.OperatorPeers) != tc.wantPeers {
				t.Errorf("OperatorPeers = %v, want %d", cfg.OperatorPeers, tc.wantPeers)
			}
		})
	}
}

// web.env's quoting is viper's dotenv codec's (gotenv): what docs/Deployment.md
// documents.
func TestLoadWebEnvQuoting(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOMELAB_TEST_HOST", "expanded")
	configDir(t, map[string]string{WebEnvFile: strings.Join([]string{
		"# a comment",
		"export CONFIG_REPO_REF=main",
		`STORE_PATH="/var/lib/homelab ops/web.db"`,
		`FLASHER_TOOL_PATH='/opt/$HOMELAB_TEST_HOST/flasher-tool'`,
		`CONFIG_REPO_URL=git://$HOMELAB_TEST_HOST/fleet.git`,
	}, "\n")})
	cfg := mustLoad(t)
	for _, c := range []struct{ name, got, want string }{
		{"export prefix", cfg.ConfigRepoRef, "main"},
		{"double quotes", cfg.StorePath, "/var/lib/homelab ops/web.db"},
		{"single quotes are literal", cfg.FlasherToolPath, "/opt/$HOMELAB_TEST_HOST/flasher-tool"},
		{"unquoted $VAR expands from the environment", cfg.ConfigRepoURL, "git://expanded/fleet.git"},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// Fail fast (#67): every missing or invalid setting is named, all at once.
func TestLoadFailsFast(t *testing.T) {
	cases := []struct {
		name    string
		webEnv  string
		files   map[string]string
		env     map[string]string
		want    []string // substrings of the error, each
		notWant string
	}{
		{name: "unknown web.env key", webEnv: "CONFIG_REPO_URL=git://h/r.git\nCONFG_REPO_REF=main\n", want: []string{"web.env: unknown setting CONFG_REPO_REF"}},
		{name: "file setting in web.env", webEnv: "CLIENT_CERT_PATH=/x.crt\n", want: []string{"web.env: CLIENT_CERT_PATH is set by the directory's client.crt file"}},
		{name: "env-only setting in web.env", webEnv: "DEPLOYMENT_CONFIG_DIR=/etc\n", want: []string{"web.env: DEPLOYMENT_CONFIG_DIR can only be set in the environment"}},
		{name: "malformed web.env", webEnv: "CONFIG_REPO_REF=\"main\n", want: []string{"web.env:"}},
		{
			name:    "URL userinfo, token not echoed",
			webEnv:  "CONFIG_REPO_URL=https://user:s3cret@github.com/me/fleet.git\n",
			want:    []string{"CONFIG_REPO_URL: carries a credential", "#218"},
			notWant: "s3cret",
		},
		{name: "URL username only", env: map[string]string{"CONFIG_REPO_URL": "https://ghp_token@github.com/me/fleet.git"}, want: []string{"CONFIG_REPO_URL: carries a credential"}, notWant: "ghp_token"},
		{name: "ssh URL with a password", env: map[string]string{"CONFIG_REPO_URL": "ssh://git:s3cret@github.com/me/fleet.git"}, want: []string{"CONFIG_REPO_URL: carries a credential"}, notWant: "s3cret"},
		{name: "scp-style URL with a password", env: map[string]string{"CONFIG_REPO_URL": "git:s3cret@github.com:me/fleet.git"}, want: []string{"CONFIG_REPO_URL: carries a credential"}, notWant: "s3cret"},
		{
			name:  "operator peers without the endpoint they need",
			files: map[string]string{OperatorPeersFile: "10.100.0.240=" + key('a') + "\n"},
			want:  []string{"WIREGUARD_ENDPOINT: required when operator peers are listed"},
		},
		{name: "bad operator peer in the file", files: map[string]string{OperatorPeersFile: "10.100.0.5=" + key('a') + "\n"}, webEnv: "WIREGUARD_ENDPOINT=h:1\n", want: []string{"WIREGUARD_OPERATOR_PEERS (operator-peers): ", "outside the operator range"}},
		{name: "bad operator peer in the env", env: map[string]string{"WIREGUARD_OPERATOR_PEERS": "nope", "WIREGUARD_ENDPOINT": "h:1"}, want: []string{"WIREGUARD_OPERATOR_PEERS: "}},
		{name: "endpoint without a port", webEnv: "WIREGUARD_ENDPOINT=webapp.example.net\n", want: []string{"WIREGUARD_ENDPOINT: \"webapp.example.net\""}},
		{name: "endpoint without a host", webEnv: "WIREGUARD_ENDPOINT=:51820\n", want: []string{"WIREGUARD_ENDPOINT: \":51820\": missing host"}},
		{name: "WIREGUARD_PORT not a number", webEnv: "WIREGUARD_PORT=wg\n", want: []string{`WIREGUARD_PORT: "wg" is not a port number`}},
		{name: "WIREGUARD_PORT out of range", webEnv: "WIREGUARD_PORT=70000\n", want: []string{`WIREGUARD_PORT: "70000"`}},
		{name: "CONFIG_SYNC_INTERVAL not a duration", webEnv: "CONFIG_SYNC_INTERVAL=5\n", want: []string{`CONFIG_SYNC_INTERVAL: "5" is not a duration`}},
		{name: "CONFIG_SYNC_INTERVAL zero", webEnv: "CONFIG_SYNC_INTERVAL=0s\n", want: []string{`CONFIG_SYNC_INTERVAL: "0s" must be positive`}},
		{name: "bad listen address", env: map[string]string{"API_HOST_LISTEN_ADDR": "8080", "HEALTH_LISTEN_ADDR": "localhost"}, want: []string{"API_HOST_LISTEN_ADDR: \"8080\"", "HEALTH_LISTEN_ADDR: \"localhost\""}},
		{name: "CONFIG_REPO_PRIVATE not true or false", webEnv: "CONFIG_REPO_PRIVATE=yes\n", want: []string{`CONFIG_REPO_PRIVATE: "yes": want "true" or "false"`}},
		{name: "CONFIG_REPO_PRIVATE in capitals", env: map[string]string{"CONFIG_REPO_PRIVATE": "TRUE"}, want: []string{`CONFIG_REPO_PRIVATE: "TRUE"`}},
		{name: "missing addressing file", webEnv: "NETWORK_ADDRESSING_PATH=/nonexistent/networks.yaml\n", want: []string{"NETWORK_ADDRESSING_PATH: "}},
		{name: "missing cert and image files", env: map[string]string{"CLIENT_CERT_PATH": "/nonexistent/client.crt", "BASE_IMAGE_PATH": "/nonexistent/base.img"}, want: []string{"CLIENT_CERT_PATH: ", "BASE_IMAGE_PATH: "}},
		{
			name:   "every problem named at once",
			webEnv: "WIREGUARD_PORT=wg\nBOGUS=1\nCONFIG_SYNC_INTERVAL=soon\n",
			files:  map[string]string{OperatorPeersFile: "10.100.0.240=" + key('a') + "\n"},
			want:   []string{"WIREGUARD_PORT:", "unknown setting BOGUS", "CONFIG_SYNC_INTERVAL:", "WIREGUARD_ENDPOINT: required"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			files := map[string]string{WebEnvFile: tc.webEnv}
			for k, v := range tc.files {
				files[k] = v
			}
			configDir(t, files)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil {
				t.Fatal("Load = nil error, want a failure")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("error echoes %q:\n%v", tc.notWant, err)
			}
		})
	}
}

// CONFIG_REPO_PRIVATE and NETWORK_ADDRESSING_PATH (#199) load from web.env,
// and an env var set to "" puts CONFIG_REPO_PRIVATE back to its default.
func TestLoadNetworkAddressing(t *testing.T) {
	clearEnv(t)
	networks := filepath.Join(t.TempDir(), "networks.yaml")
	if err := os.WriteFile(networks, []byte("kind: Network\nname: lab\ncidr: 10.0.2.0/24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir(t, map[string]string{WebEnvFile: "CONFIG_REPO_PRIVATE=true\nNETWORK_ADDRESSING_PATH=" + networks + "\n"})
	if cfg := mustLoad(t); !cfg.ConfigRepoPrivate || cfg.NetworkAddressingPath != networks {
		t.Errorf("from web.env: ConfigRepoPrivate = %v, NetworkAddressingPath = %q; want true, %q", cfg.ConfigRepoPrivate, cfg.NetworkAddressingPath, networks)
	}
	t.Setenv("CONFIG_REPO_PRIVATE", "")
	if cfg := mustLoad(t); cfg.ConfigRepoPrivate {
		t.Error("CONFIG_REPO_PRIVATE set empty in the env: ConfigRepoPrivate = true, want the default false")
	}
	t.Setenv("CONFIG_REPO_PRIVATE", "false")
	if cfg := mustLoad(t); cfg.ConfigRepoPrivate {
		t.Error("CONFIG_REPO_PRIVATE=false in the env: ConfigRepoPrivate = true, want false")
	}
}

// URLs with no credential, every form go-git takes, pass: including SSH
// URLs with a user, which is a login name, not a secret.
func TestLoadAcceptsRepoURLsWithoutCredentials(t *testing.T) {
	for _, u := range []string{
		"https://github.com/me/fleet.git", "git://config-repo/fleet.git", "/root/fleet-repo.git", "file:///srv/fleet.git", "github.com:me/fleet.git",
		"git@github.com:me/fleet.git", "ssh://git@github.com/me/fleet.git", "/srv/repos/fleet@v2.git",
	} {
		t.Run(u, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("CONFIG_REPO_URL", u)
			mustLoad(t)
		})
	}
}

func TestLoadRefusesRetiredPortWithoutAListener(t *testing.T) {
	cases := []struct {
		name, endpoint, hostAddr string
		wantErr                  bool
	}{
		{"no API listener", "", "", true},
		{"with the tunnel", "203.0.113.1:51820", "", false},
		{"with a host listener", "", ":8080", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PORT", "8080")
			t.Setenv("WIREGUARD_ENDPOINT", tc.endpoint)
			t.Setenv("API_HOST_LISTEN_ADDR", tc.hostAddr)
			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load err = %v, want error: %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "API_HOST_LISTEN_ADDR=:8080") {
				t.Errorf("error %q doesn't name the opt-in that keeps the old port", err)
			}
			if err == nil && cfg.RetiredPort != "8080" {
				t.Errorf("RetiredPort = %q, want 8080 for cmd/web to log", cfg.RetiredPort)
			}
		})
	}
}

func TestLoadDirectoryMustExist(t *testing.T) {
	clearEnv(t)
	t.Setenv("DEPLOYMENT_CONFIG_DIR", filepath.Join(t.TempDir(), "missing"))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DEPLOYMENT_CONFIG_DIR: ") {
		t.Fatalf("Load = %v, want a DEPLOYMENT_CONFIG_DIR error", err)
	}
}

// A directory without web.env is valid: every setting from env or defaults.
func TestLoadDirectoryWithoutWebEnv(t *testing.T) {
	clearEnv(t)
	dir := configDir(t, map[string]string{ClientCertFile: "cert"})
	if cfg := mustLoad(t); cfg.ClientCertPath != filepath.Join(dir, ClientCertFile) {
		t.Errorf("ClientCertPath = %q, want the directory's client.crt", cfg.ClientCertPath)
	}
}

// secrets/ is 0700 and everything under it 0600 or stricter (#241).
func TestLoadChecksSecretModes(t *testing.T) {
	cases := []struct {
		name             string
		dirMode, keyMode os.FileMode
		want             string // "" means Load succeeds
	}{
		{"0700 and 0600", 0o700, 0o600, ""},
		{"0700 and 0400", 0o700, 0o400, ""},
		{"group-readable directory", 0o750, 0o600, "secrets: mode 0750"},
		{"world-readable key", 0o700, 0o644, "secrets/wireguard.key: mode 0644"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			dir := configDir(t, map[string]string{WireGuardKeyFile: "key"})
			if err := os.Chmod(filepath.Join(dir, WireGuardKeyFile), tc.keyMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(dir, SecretsDir), tc.dirMode); err != nil {
				t.Fatal(err)
			}
			_, err := Load()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Load: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Load = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestStripComments(t *testing.T) {
	got := StripComments("# header\n\n10.100.0.240=k1= # laptop\n  10.100.0.241=k2=\n#10.100.0.242=old\n")
	if want := "10.100.0.240=k1=\n10.100.0.241=k2="; got != want {
		t.Errorf("StripComments = %q, want %q", got, want)
	}
}

// TestLoadReadsOnlyListedSettings is the other half of #241's "a test fails
// when cmd/web reads a setting the contract doesn't list" (cmd/web's
// TestReadsSettingsOnlyThroughDeployconfig is the first): every name Load
// reads is a row of Settings, and every row is read. Load reads each
// setting whatever the values, so two loads, with and without a directory,
// see every read.
func TestLoadReadsOnlyListedSettings(t *testing.T) {
	read := map[string]bool{}
	readHook = func(name string) { read[name] = true }
	t.Cleanup(func() { readHook = nil })

	clearEnv(t)
	mustLoad(t)
	configDir(t, map[string]string{ClientCertFile: "cert", OperatorPeersFile: ""})
	mustLoad(t)

	listed := map[string]bool{}
	for _, s := range Settings {
		listed[s.Name] = true
		if !read[s.Name] {
			t.Errorf("Settings lists %s, but Load never reads it", s.Name)
		}
	}
	for name := range read {
		if !listed[name] {
			t.Errorf("Load reads %s, which Settings (the contract) doesn't list: add it there and to docs/Deployment.md", name)
		}
	}
}

// Every row of the contract is documented in docs/Deployment.md's settings
// table, and the table documents nothing the contract lacks.
func TestDeploymentDocListsEverySetting(t *testing.T) {
	doc, err := os.ReadFile("../../docs/Deployment.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `([A-Z][A-Z0-9_]+)` \\|")
	documented := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(string(doc), -1) {
		documented[m[1]] = true
	}
	listed := map[string]bool{}
	for _, s := range Settings {
		listed[s.Name] = true
		if !documented[s.Name] {
			t.Errorf("docs/Deployment.md has no settings-table row for %s", s.Name)
		}
	}
	for name := range documented {
		if !listed[name] {
			t.Errorf("docs/Deployment.md documents %s, which Settings doesn't list", name)
		}
	}
}

// The committed example directory loads as it stands.
func TestExampleWebappLoads(t *testing.T) {
	clearEnv(t)
	t.Setenv("DEPLOYMENT_CONFIG_DIR", "../../examples/webapp")
	cfg := mustLoad(t)
	if cfg.WireGuardEndpoint != "webapp.example.net:51820" || cfg.ConfigRepoURL == "" || cfg.SyncInterval != 5*time.Minute {
		t.Errorf("examples/webapp loaded as %+v, want its web.env's settings", cfg)
	}
}

func ptr(s string) *string { return &s }
