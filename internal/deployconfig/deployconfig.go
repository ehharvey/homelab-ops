// Package deployconfig loads the web app's deployment config (#241,
// docs/Decisions.md §29): every setting cmd/web reads, from the environment
// and, when DEPLOYMENT_CONFIG_DIR is set, from a directory of files, into one
// typed Config. Load validates everything at once, so a bad deployment stops
// at startup with one message naming each problem (#67), instead of failing
// later in whichever code path first reads a setting.
//
// The contract is the Settings table: cmd/web reads nothing else (a test in
// cmd/web refuses any direct environment read there, and one here refuses a
// read of a name the table doesn't list), and docs/Deployment.md documents
// every row (a test checks that too).
//
// The directory (docs/Deployment.md has the full layout):
//
//	web.env          scalar settings, KEY=VALUE, the env var names
//	client.crt          CLIENT_CERT_PATH: the break-glass cert's public half
//	operator-certs.pem  OPERATOR_CERTS_PATH: the operator certs the API
//	                    requires (#397); unset, client.crt alone
//	operator-peers      WIREGUARD_OPERATOR_PEERS, one <addr>=<pubkey> per line
//	secrets/            0700; every secret file, each 0600
//	  wireguard.key     the operator-supplied WireGuard identity: read for
//	                    the API's TLS key when the tunnel is off (#397);
//	                    #197 will load it for the tunnel
//
// An environment variable overrides the same setting from the directory, and
// one set to the empty string still overrides it: empty then means the
// setting's default.
package deployconfig

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/ehharvey/homelab-ops/internal/webtls"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// Source says where a setting may come from besides the environment, which
// can set every one of them.
type Source int

const (
	// WebEnv settings may also be set in the directory's web.env.
	WebEnv Source = iota
	// File settings come from their own file in the directory, never web.env.
	File
	// EnvOnly settings come only from the environment.
	EnvOnly
)

// Setting is one row of the deployment config contract.
type Setting struct {
	// Name is the environment variable, and web.env's key for a WebEnv
	// setting.
	Name   string
	Source Source
	// File is the directory's file for a File setting.
	File string
}

// The directory's fixed entries.
const (
	WebEnvFile        = "web.env"
	ClientCertFile    = "client.crt"
	OperatorCertsFile = "operator-certs.pem"
	OperatorPeersFile = "operator-peers"
	SecretsDir        = "secrets"
	// WireGuardKeyFile is the operator-supplied WireGuard identity. Load
	// reads it only when an API listener serves TLS with the tunnel off, for
	// the TLS key (#397); #197 will load it for the tunnel too.
	WireGuardKeyFile = SecretsDir + "/wireguard.key"
)

// Settings is the contract: every setting cmd/web reads, in
// docs/Deployment.md's order. Adding a setting means a row here, a field in
// Config, a read in Load and a row in docs/Deployment.md; the tests catch a
// missing one.
var Settings = []Setting{
	{Name: "DEPLOYMENT_CONFIG_DIR", Source: EnvOnly},
	{Name: "WIREGUARD_ENDPOINT", Source: WebEnv},
	{Name: "WIREGUARD_PORT", Source: WebEnv},
	{Name: "WIREGUARD_OPERATOR_PEERS", Source: File, File: OperatorPeersFile},
	{Name: "API_HOST_LISTEN_ADDR", Source: WebEnv},
	{Name: "API_HOST_PLAIN_HTTP", Source: WebEnv},
	{Name: "HEALTH_LISTEN_ADDR", Source: WebEnv},
	{Name: "PORT", Source: WebEnv},
	{Name: "CONFIG_REPO_URL", Source: WebEnv},
	{Name: "CONFIG_REPO_REF", Source: WebEnv},
	{Name: "CONFIG_SYNC_INTERVAL", Source: WebEnv},
	{Name: "CONFIG_REPO_PRIVATE", Source: WebEnv},
	{Name: "NETWORK_ADDRESSING_PATH", Source: WebEnv},
	{Name: "STORE_PATH", Source: WebEnv},
	{Name: "CLIENT_CERT_PATH", Source: File, File: ClientCertFile},
	{Name: "OPERATOR_CERTS_PATH", Source: File, File: OperatorCertsFile},
	{Name: "BASE_IMAGE_PATH", Source: WebEnv},
	{Name: "FLASHER_TOOL_PATH", Source: WebEnv},
}

// Defaults that Load fills in for an unset or empty setting.
const (
	DefaultHealthListenAddr = ":8081"
	DefaultWireGuardPort    = 51820
	DefaultStorePath        = ":memory:"
)

// Config is the web app's whole deployment config, typed and validated. A
// zero string means the setting is off or unset, unless its doc says it has a
// default, which Load has already applied.
type Config struct {
	// Dir is DEPLOYMENT_CONFIG_DIR, or "" when only the environment is used.
	Dir string

	// WireGuardEndpoint is the host:port nodes dial; "" means no tunnel.
	WireGuardEndpoint string
	// WireGuardPort is the tunnel's UDP listen port (0: the OS picks one).
	WireGuardPort int
	// OperatorPeers may call the API over the tunnel.
	OperatorPeers []wireguard.OperatorPeer

	// APIHostListenAddr opts in to the host listener, which serves TLS and
	// requires an operator cert (#397) unless APIHostPlainHTTP.
	APIHostListenAddr string
	// APIHostPlainHTTP makes the host listener plain HTTP with no
	// authentication: dev and the validate suite's opt-in. It does nothing
	// without APIHostListenAddr.
	APIHostPlainHTTP bool
	HealthListenAddr string
	// RetiredPort is a set PORT, which #195 retired. Load accepts it only
	// with an API listener configured; cmd/web logs that it isn't read.
	RetiredPort string

	ConfigRepoURL string
	ConfigRepoRef string
	// SyncInterval is the background poll interval; 0 means sync only on
	// POST /sync.
	SyncInterval time.Duration
	// ConfigRepoPrivate allows network addressing in git (docs/Decisions.md
	// §28, #199). False, the default, rejects it at sync: a public repo must
	// not publish the LAN's addresses.
	ConfigRepoPrivate bool
	// NetworkAddressingPath is the file of kind: Network documents that
	// addresses the state-addressed networks (§28, #199); "" means none.
	NetworkAddressingPath string

	StorePath      string
	ClientCertPath string
	// OperatorCertsPath is OPERATOR_CERTS_PATH as set; "" falls back to
	// ClientCertPath.
	OperatorCertsPath string
	// OperatorCerts are the certs a TLS API listener requires a client to
	// present one of (#397), parsed from the file OperatorCertsFrom names.
	// Loaded only when TLSAPI, or when OPERATOR_CERTS_PATH is set. Empty
	// with TLSAPI only while no TLS listener could admit anyone (the tunnel
	// with no operator peers), and then the TLS listeners refuse everyone.
	OperatorCerts []*x509.Certificate
	// OperatorCertsFrom names the setting OperatorCerts came from:
	// OPERATOR_CERTS_PATH, or CLIENT_CERT_PATH as its fallback; "" when
	// none.
	OperatorCertsFrom string
	// WireGuardKey is the identity in secrets/wireguard.key, loaded only
	// when HostOnlyTLS: the API's TLS key is derived from it (#397). With
	// the tunnel on, the TLS key comes from the tunnel's own identity
	// instead. Zero when not loaded.
	WireGuardKey wireguard.PrivateKey

	BaseImagePath   string
	FlasherToolPath string
}

// admitsOperators reports whether a TLS API listener could admit anyone, so
// it needs operator certs to admit them by (#397): the tunnel's once an
// operator peer is listed, or a TLS host listener. With the tunnel on and no
// operator peers, its peer gate already refuses every request, so no cert is
// required: that is the committed examples/webapp, before its operator adds
// a peer and the cert together. cmd/web warns of both.
func (c Config) admitsOperators() bool {
	return c.WireGuardEndpoint != "" && len(c.OperatorPeers) > 0 || c.HostTLS()
}

// TLSAPI reports whether any API listener serves TLS (#397): the tunnel
// listener always does, and the host listener unless APIHostPlainHTTP.
func (c Config) TLSAPI() bool {
	return c.WireGuardEndpoint != "" || c.HostTLS()
}

// HostTLS reports whether the host listener is on and serves TLS.
func (c Config) HostTLS() bool {
	return c.APIHostListenAddr != "" && !c.APIHostPlainHTTP
}

// HostOnlyTLS reports whether the host listener serves TLS with no tunnel,
// so the TLS key can't come from the tunnel's identity and is derived from
// secrets/wireguard.key instead (operator decision on #397, 2026-11-10).
func (c Config) HostOnlyTLS() bool {
	return c.HostTLS() && c.WireGuardEndpoint == ""
}

// Load reads the deployment config from the environment and, when
// DEPLOYMENT_CONFIG_DIR is set, the directory it names, and validates it. Its
// error names every missing or invalid setting, one per line.
func Load() (Config, error) {
	v := viper.New()
	v.SetConfigType("env")
	v.AutomaticEnv()
	// An env var set to "" still overrides the file (#241).
	v.AllowEmptyEnv(true)
	l := &loader{v: v, read: readHook}

	l.cfg.Dir = l.get("DEPLOYMENT_CONFIG_DIR")
	if l.cfg.Dir != "" {
		l.readDir()
	}

	c := &l.cfg
	c.WireGuardEndpoint = l.get("WIREGUARD_ENDPOINT")
	c.WireGuardPort = l.port("WIREGUARD_PORT", DefaultWireGuardPort)
	c.OperatorPeers = l.operatorPeers()
	c.APIHostListenAddr = l.get("API_HOST_LISTEN_ADDR")
	c.APIHostPlainHTTP = l.boolean("API_HOST_PLAIN_HTTP")
	c.HealthListenAddr = l.get("HEALTH_LISTEN_ADDR")
	if c.HealthListenAddr == "" {
		c.HealthListenAddr = DefaultHealthListenAddr
	}
	c.RetiredPort = l.get("PORT")
	c.ConfigRepoURL = l.get("CONFIG_REPO_URL")
	c.ConfigRepoRef = l.get("CONFIG_REPO_REF")
	c.SyncInterval = l.duration("CONFIG_SYNC_INTERVAL")
	c.ConfigRepoPrivate = l.boolean("CONFIG_REPO_PRIVATE")
	c.NetworkAddressingPath = l.get("NETWORK_ADDRESSING_PATH")
	c.StorePath = l.get("STORE_PATH")
	if c.StorePath == "" {
		c.StorePath = DefaultStorePath
	}
	c.ClientCertPath = l.fileSetting("CLIENT_CERT_PATH", ClientCertFile)
	c.OperatorCertsPath = l.fileSetting("OPERATOR_CERTS_PATH", OperatorCertsFile)
	c.BaseImagePath = l.get("BASE_IMAGE_PATH")
	c.FlasherToolPath = l.get("FLASHER_TOOL_PATH")

	l.validate()
	if len(l.errs) > 0 {
		return Config{}, fmt.Errorf("invalid deployment config (docs/Deployment.md):\n%w", errors.Join(l.errs...))
	}
	return l.cfg, nil
}

type loader struct {
	v    *viper.Viper
	cfg  Config
	errs []error
	// read, when non-nil, is told every setting name Load reads, so a test
	// can hold the reads to Settings.
	read func(name string)
}

// readHook is the read hook every Load starts with; nil outside tests.
var readHook func(name string)

func (l *loader) fail(name, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", name, fmt.Sprintf(format, args...)))
}

// get reads one setting: the environment first (set, even to ""), then
// web.env. It and envSet are Load's only reads, and both report the name to
// the read hook, which TestLoadReadsOnlyListedSettings holds to Settings.
func (l *loader) get(name string) string {
	if l.read != nil {
		l.read(name)
	}
	return l.v.GetString(name)
}

// envSet reports whether the environment sets name, even to "".
func (l *loader) envSet(name string) bool {
	if l.read != nil {
		l.read(name)
	}
	_, ok := os.LookupEnv(name)
	return ok
}

// readDir checks the directory and reads web.env into viper, below the env.
func (l *loader) readDir() {
	dir := l.cfg.Dir
	info, err := os.Stat(dir)
	if err != nil {
		l.fail("DEPLOYMENT_CONFIG_DIR", "%v", err)
		return
	}
	if !info.IsDir() {
		l.fail("DEPLOYMENT_CONFIG_DIR", "%s is not a directory", dir)
		return
	}
	l.checkSecrets()

	f, err := os.Open(filepath.Join(dir, WebEnvFile)) //nolint:gosec // G304: the path is operator-supplied deployment config
	if errors.Is(err, fs.ErrNotExist) {
		return // every setting from env or the defaults
	}
	if err != nil {
		l.fail(WebEnvFile, "%v", err)
		return
	}
	defer f.Close() //nolint:errcheck // read-only
	if err := l.v.ReadConfig(f); err != nil {
		l.fail(WebEnvFile, "%v", err)
		return
	}
	for _, key := range l.v.AllKeys() {
		name := strings.ToUpper(key)
		s, ok := lookup(name)
		switch {
		case !ok:
			l.fail(WebEnvFile, "unknown setting %s (docs/Deployment.md lists them)", name)
		case s.Source == File:
			l.fail(WebEnvFile, "%s is set by the directory's %s file, not web.env", name, s.File)
		case s.Source == EnvOnly:
			l.fail(WebEnvFile, "%s can only be set in the environment", name)
		}
	}
}

// checkSecrets holds secrets/ to its modes, when it exists: the directory
// 0700 and everything under it 0600 or stricter, so nothing secret is
// readable beyond its owner (#241). Only permission bits are checked, not
// ownership.
func (l *loader) checkSecrets() {
	root := filepath.Join(l.cfg.Dir, SecretsDir)
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return
	}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(l.cfg.Dir, path)
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			want := "0600"
			if info.IsDir() {
				want = "0700"
			}
			l.fail(rel, "mode %04o is readable beyond its owner; want %s", perm, want)
		}
		return nil
	})
	if err != nil {
		l.fail(SecretsDir, "%v", err)
	}
}

// fileSetting reads a File setting: the environment if set there, else the
// directory's file if it exists, else unset.
func (l *loader) fileSetting(name, file string) string {
	if l.envSet(name) || l.cfg.Dir == "" {
		return l.get(name)
	}
	path := filepath.Join(l.cfg.Dir, file)
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			l.fail(name, "%s: %v", file, err)
		}
		return ""
	}
	return path
}

// operatorPeers parses WIREGUARD_OPERATOR_PEERS from the environment, or else
// the directory's operator-peers file: one <addr>=<pubkey> per line, # starts
// a comment.
func (l *loader) operatorPeers() []wireguard.OperatorPeer {
	const name = "WIREGUARD_OPERATOR_PEERS"
	raw, from := "", name
	if l.envSet(name) || l.cfg.Dir == "" {
		raw = l.get(name)
	} else if path := l.fileSetting(name, OperatorPeersFile); path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the path is operator-supplied deployment config
		if err != nil {
			l.fail(name, "%v", err)
			return nil
		}
		raw, from = StripComments(string(b)), name+" ("+OperatorPeersFile+")"
	}
	peers, err := wireguard.ParseOperatorPeers(raw)
	if err != nil {
		l.fail(from, "%v", err)
		return nil
	}
	return peers
}

// StripComments drops everything from a # to the end of each line, and blank
// lines, for the operator-peers file. A base64 key never contains #.
func StripComments(s string) string {
	var out []string
	for line := range strings.Lines(s) {
		line, _, _ = strings.Cut(line, "#")
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// port reads a UDP/TCP port number, or def when unset or empty.
func (l *loader) port(name string, def int) int {
	raw := l.get(name)
	if raw == "" {
		return def
	}
	p, err := strconv.Atoi(raw)
	if err != nil || p < 0 || p > 65535 {
		l.fail(name, "%q is not a port number (0-65535)", raw)
		return def
	}
	return p
}

// duration reads a positive Go duration such as "5m", or 0 when unset or
// empty.
func (l *loader) duration(name string) time.Duration {
	raw := l.get(name)
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		l.fail(name, "%q is not a duration such as 5m: %v", raw, err)
		return 0
	}
	if d <= 0 {
		l.fail(name, "%q must be positive; leave it unset to sync only on POST /sync", raw)
		return 0
	}
	return d
}

// boolean reads "true" or "false", or false when unset or empty. Nothing else
// is accepted, so a typo can't silently pick either side.
func (l *loader) boolean(name string) bool {
	switch raw := l.get(name); raw {
	case "", "false":
		return false
	case "true":
		return true
	default:
		l.fail(name, "%q: want \"true\" or \"false\"", raw)
		return false
	}
}

// validate checks the settings against each other and the filesystem.
func (l *loader) validate() {
	c := &l.cfg
	if c.WireGuardEndpoint != "" {
		if err := checkHostPort(c.WireGuardEndpoint, false); err != nil {
			l.fail("WIREGUARD_ENDPOINT", "%q: %v; want host:port, a DNS name recommended", c.WireGuardEndpoint, err)
		}
	} else if len(c.OperatorPeers) > 0 {
		l.fail("WIREGUARD_ENDPOINT", "required when operator peers are listed: they reach the API over the tunnel, which only runs when WIREGUARD_ENDPOINT is set (host:port, a DNS name recommended)")
	}
	if c.APIHostListenAddr != "" {
		if err := checkHostPort(c.APIHostListenAddr, true); err != nil {
			l.fail("API_HOST_LISTEN_ADDR", "%q: %v", c.APIHostListenAddr, err)
		}
	}
	if err := checkHostPort(c.HealthListenAddr, true); err != nil {
		l.fail("HEALTH_LISTEN_ADDR", "%q: %v", c.HealthListenAddr, err)
	}
	if c.RetiredPort != "" && c.WireGuardEndpoint == "" && c.APIHostListenAddr == "" {
		l.fail("PORT", "%q is no longer read (#195), and no API listener is configured: set WIREGUARD_ENDPOINT and WIREGUARD_OPERATOR_PEERS (the default, tunnel-only posture), or API_HOST_LISTEN_ADDR=:%s to keep serving plain HTTP on that port", c.RetiredPort, c.RetiredPort)
	}
	if hasCredential(c.ConfigRepoURL) {
		// The URL itself isn't echoed: it holds the credential.
		l.fail("CONFIG_REPO_URL", "carries a credential in its userinfo (any user@ or user:token@ on a non-SSH URL, where a token can stand in for the user, or a user:password@ on an SSH one); credentials never go in the URL or web.env, and git credentials are #218's, under secrets/")
	}
	bad := map[string]bool{}
	for _, f := range []struct{ name, path string }{
		{"CLIENT_CERT_PATH", c.ClientCertPath},
		{"OPERATOR_CERTS_PATH", c.OperatorCertsPath},
		{"BASE_IMAGE_PATH", c.BaseImagePath},
		{"NETWORK_ADDRESSING_PATH", c.NetworkAddressingPath},
	} {
		if f.path == "" {
			continue
		}
		if info, err := os.Stat(f.path); err != nil {
			l.fail(f.name, "%v", err)
			bad[f.name] = true
		} else if !info.Mode().IsRegular() {
			l.fail(f.name, "%s is not a regular file", f.path)
			bad[f.name] = true
		}
	}
	l.operatorCerts(bad)
	if c.HostOnlyTLS() {
		l.hostTLSIdentity()
	}
}

// operatorCerts loads the certs a TLS API listener requires of clients
// (#397): OPERATOR_CERTS_PATH's, or else CLIENT_CERT_PATH's, the break-glass
// cert (operator decision, 2026-11-10). Without a TLS API listener only an
// explicit OPERATOR_CERTS_PATH is read, so a bad one still fails fast. With
// neither set, startup fails naming both only while a TLS listener could
// admit someone (admitsOperators). bad holds the file settings that already
// failed.
func (l *loader) operatorCerts(bad map[string]bool) {
	c := &l.cfg
	name, path := "OPERATOR_CERTS_PATH", c.OperatorCertsPath
	if path == "" {
		name, path = "CLIENT_CERT_PATH", c.ClientCertPath
		if !c.TLSAPI() {
			return
		}
	}
	if path == "" {
		if !c.admitsOperators() {
			return
		}
		l.fail("OPERATOR_CERTS_PATH", "no operator cert for the API to require: %s serves TLS, and admits only clients presenting a listed operator cert. Set OPERATOR_CERTS_PATH (the directory's %s), or CLIENT_CERT_PATH (its %s), which it falls back to; or, for dev only, API_HOST_PLAIN_HTTP=true with the tunnel off", tlsListeners(*c), OperatorCertsFile, ClientCertFile)
		return
	}
	if bad[name] {
		return
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the path is operator-supplied deployment config
	if err != nil {
		l.fail(name, "%v", err)
		return
	}
	certs, err := webtls.ParseCertsPEM(b)
	if err != nil {
		l.fail(name, "%s: %v", path, err)
		return
	}
	c.OperatorCerts, c.OperatorCertsFrom = certs, name
}

// tlsListeners names the API listeners that serve TLS, for an error.
func tlsListeners(c Config) string {
	switch {
	case c.WireGuardEndpoint != "" && c.HostTLS():
		return "the tunnel API listener (WIREGUARD_ENDPOINT) and the host listener (API_HOST_LISTEN_ADDR)"
	case c.WireGuardEndpoint != "":
		return "the tunnel API listener (WIREGUARD_ENDPOINT)"
	default:
		return "the host listener (API_HOST_LISTEN_ADDR)"
	}
}

// hostTLSIdentity loads secrets/wireguard.key for a host listener serving
// TLS with the tunnel off. The API's TLS key is derived from the web app's
// WireGuard identity, and with no tunnel the operator-supplied file is the
// only one, so it is required (operator decision on #397, 2026-11-10): a
// key made up at each start would change the pinned fingerprint.
func (l *loader) hostTLSIdentity() {
	c := &l.cfg
	const why = "required while API_HOST_LISTEN_ADDR serves TLS with the tunnel off (WIREGUARD_ENDPOINT unset): the API's TLS key is derived from the web app's WireGuard identity (#397)"
	if c.Dir == "" {
		l.fail(WireGuardKeyFile, "%s; set DEPLOYMENT_CONFIG_DIR to a directory holding one (a base64 private key, as wg genkey prints it), or, for dev only, API_HOST_PLAIN_HTTP=true", why)
		return
	}
	path := filepath.Join(c.Dir, WireGuardKeyFile)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		l.fail(WireGuardKeyFile, "missing, and %s; put a base64 private key there, as wg genkey prints it (mode 0600)", why)
		return
	}
	key, err := wireguard.ReadPrivateKeyFile(path)
	if err != nil {
		l.fail(WireGuardKeyFile, "%v", err)
		return
	}
	c.WireGuardKey = key
}

// checkHostPort accepts host:port with a numeric port; emptyHost allows
// ":port" for a listen address.
func checkHostPort(s string, emptyHost bool) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return err
	}
	if host == "" && !emptyHost {
		return errors.New("missing host")
	}
	if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("port %q is not a number (0-65535)", port)
	}
	return nil
}

// hasCredential reports whether a repo URL embeds a credential. Over SSH
// (ssh:// or scp-style [user@]host:path, as go-git accepts) the user is a
// login name, such as git@github.com's git, and the key comes from ssh-agent,
// so only a password after it is one. On any other URL, all userinfo is: an
// https token is often given as the user alone (https://<token>@host). A
// go-git local path that url.Parse can't read carries none.
func hasCredential(raw string) bool {
	if raw == "" {
		return false
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		if u.User == nil {
			return false
		}
		if u.Scheme == "ssh" {
			_, hasPassword := u.User.Password()
			return hasPassword
		}
		return true
	}
	// scp-style: [user[:password]@]host:path. A path with a slash before
	// its @ (a local path that happens to hold one) isn't scp-style.
	user, rest, ok := strings.Cut(raw, "@")
	return ok && !strings.Contains(user, "/") && strings.Contains(rest, ":") && strings.Contains(user, ":")
}

func lookup(name string) (Setting, bool) {
	for _, s := range Settings {
		if s.Name == name {
			return s, true
		}
	}
	return Setting{}, false
}
