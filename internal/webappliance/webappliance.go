// Package webappliance renders the web app appliance's NoCloud seed (#243,
// docs/Decisions.md §29, docs/Deployment.md § The appliance): the user-data
// and meta-data that, on a FAT volume labelled CIDATA beside the generic
// NixOS image (deploy/nixos/), turn that image into this operator's
// appliance at first boot.
//
// user-data is a cloud-config whose write_files carries, at the paths
// deploy/nixos/appliance.nix reads:
//
//	/etc/homelab-ops/compose.yaml             deploy/compose.yaml, its image pinned to Options.Image
//	/etc/homelab-ops/appliance/settings.json  the host settings: reboot time, SSH public keys
//	/etc/homelab-ops/web/...                  every file of the deployment config directory, with its mode
//
// Its bootcmd runs first, once per seed: it empties /etc/homelab-ops/web, so a
// re-seed is the whole directory and a file dropped from --config-dir doesn't
// linger, then creates the config directory's directories with their modes,
// so secrets/ is 0700 before its files land (cloud-init would make a missing
// parent 0755, which deployconfig.Load refuses).
//
// What the seed carries of the config directory (every file but dot entries,
// editor backups and, with NoSecrets, secrets/) is also exactly what Render
// checks: it stages those files in a temporary directory and runs the web
// app's own startup checks on that.
package webappliance

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"github.com/ehharvey/homelab-ops/deploy"
	"github.com/ehharvey/homelab-ops/internal/deployconfig"
)

// Paths on the appliance, as deploy/nixos/appliance.nix names them.
const (
	EtcDir       = "/etc/homelab-ops"
	ConfigDir    = EtcDir + "/web"
	ComposePath  = EtcDir + "/compose.yaml"
	SettingsPath = EtcDir + "/appliance/settings.json"
)

// Defaults.
const (
	// DefaultRebootTime is when the appliance upgrades and may reboot, in
	// its local time.
	DefaultRebootTime = "04:00"
	// WireGuardPort is the UDP port deploy/compose.yaml publishes, and the
	// only one the appliance opens. web.env must leave WIREGUARD_PORT at it.
	WireGuardPort = deployconfig.DefaultWireGuardPort
	// Hostname is the appliance's hostname (appliance.nix's default).
	Hostname = "homelab-appliance"
)

// Options are render-appliance-seed's flags.
type Options struct {
	// ConfigDir is the deployment config directory (docs/Deployment.md § The
	// directory) to carry inline.
	ConfigDir string
	// NoSecrets leaves out exactly secrets/; the appliance's web service
	// waits until the operator copies it in.
	NoSecrets bool
	// Image is the web image, pinned by digest: <repo>@sha256:<hex>, or a
	// bare sha256:<hex> for deploy/compose.yaml's repo. "" keeps
	// deploy/compose.yaml's pin.
	Image string
	// RebootTime is HH:MM, the appliance's local time; "" is
	// DefaultRebootTime.
	RebootTime string
	// SSHAuthorizedKeys are public keys allowed to log in as root over SSH.
	// None, the default, leaves sshd off and port 22 closed.
	SSHAuthorizedKeys []string
}

// Seed is a rendered NoCloud seed.
type Seed struct {
	UserData []byte
	MetaData []byte
	// Skipped are the config directory's entries left out as clutter: dot
	// entries (.git/, .DS_Store) and editor backups (name~), relative to it.
	Skipped []string
}

// Settings is settings.json: appliance.nix's homelab.appliance.settings,
// option by option.
type Settings struct {
	RebootTime        string   `json:"rebootTime"`
	SSHAuthorizedKeys []string `json:"sshAuthorizedKeys"`
}

var (
	digestRe     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	imageRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
	rebootTimeRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// Render renders the seed. It refuses a config directory the appliance
// couldn't start from: one deployconfig.LoadDir refuses, read as the seed
// ships it with deploy/compose.yaml's environment; secrets/ missing without
// NoSecrets, or NoSecrets without SSH keys (either way the web service would
// wait for secrets/ forever: with SSH off nothing can copy it in); a file
// that isn't regular; a NETWORK_ADDRESSING_PATH, BASE_IMAGE_PATH or
// CLIENT_CERT_PATH that doesn't name a file the seed carries under
// ConfigDir, the only directory of operator files the appliance's web
// container mounts; no WIREGUARD_ENDPOINT (the appliance serves the API only
// through the tunnel); or WIREGUARD_PORT moved off the published port. Each
// SSH key is held to ParseAuthorizedKeys' rules.
func Render(o Options) (Seed, error) {
	rebootTime := o.RebootTime
	if rebootTime == "" {
		rebootTime = DefaultRebootTime
	}
	if !rebootTimeRe.MatchString(rebootTime) {
		return Seed{}, fmt.Errorf("reboot time %q: want HH:MM, 00:00 to 23:59", rebootTime)
	}
	for i, k := range o.SSHAuthorizedKeys {
		if err := checkAuthorizedKey(k); err != nil {
			return Seed{}, fmt.Errorf("SSH authorized key %d: %w", i+1, err)
		}
	}
	if o.NoSecrets && len(o.SSHAuthorizedKeys) == 0 {
		return Seed{}, errors.New("--no-secrets needs --ssh-authorized-keys: with SSH off, nothing can copy secrets/ onto the appliance, and its web service would wait for it forever")
	}
	compose, err := PinImage(deploy.ComposeYAML, o.Image)
	if err != nil {
		return Seed{}, err
	}
	env, err := composeEnv(compose)
	if err != nil {
		return Seed{}, err
	}
	entries, skipped, err := readConfigDir(o.ConfigDir, o.NoSecrets, env)
	if err != nil {
		return Seed{}, err
	}

	keys := o.SSHAuthorizedKeys
	if keys == nil {
		keys = []string{}
	}
	settings, err := json.Marshal(Settings{RebootTime: rebootTime, SSHAuthorizedKeys: keys})
	if err != nil {
		return Seed{}, err
	}

	// Empty the config directory first, keeping the directory itself (a
	// web container started before cloud-init may have it bind-mounted):
	// the seed is all of it. -mindepth 2 and -path spare the rest of
	// EtcDir; the store is a Docker volume, outside it.
	cc := cloudConfig{Bootcmd: []command{{
		"cloud-init-per", "once", "homelab-ops-clear-web",
		"find", EtcDir, "-mindepth", "2", "-path", ConfigDir + "/*", "-delete",
	}}}
	cc.WriteFiles = append(cc.WriteFiles,
		newWriteFile(ComposePath, 0o644, compose),
		newWriteFile(SettingsPath, 0o644, append(settings, '\n')),
	)
	for i, e := range entries {
		target := ConfigDir
		if e.rel != "." {
			target = path.Join(ConfigDir, e.rel)
		}
		if e.dir {
			cc.Bootcmd = append(cc.Bootcmd, command{
				"cloud-init-per", "once", fmt.Sprintf("homelab-ops-mkdir-%d", i),
				"install", "-d", "-m", fmt.Sprintf("%04o", e.mode), target,
			})
			continue
		}
		cc.WriteFiles = append(cc.WriteFiles, newWriteFile(target, e.mode, e.content))
	}

	var body bytes.Buffer
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(cc); err != nil {
		return Seed{}, err
	}
	if err := enc.Close(); err != nil {
		return Seed{}, err
	}
	var ud bytes.Buffer
	ud.WriteString("#cloud-config\n")
	ud.WriteString("# The homelab-ops web app appliance's seed, rendered by `bootstrap\n")
	ud.WriteString("# render-appliance-seed` (#243, docs/Deployment.md § The appliance).\n")
	if o.NoSecrets {
		ud.WriteString("# Rendered with --no-secrets: secrets/ is left out, and the web app\n")
		ud.WriteString("# starts once it is copied to " + ConfigDir + "/secrets.\n")
	} else {
		ud.WriteString("# It holds secrets/ in plaintext: keep it private, and remove the stick\n")
		ud.WriteString("# after the first boot.\n")
	}
	ud.Write(body.Bytes())

	sum := sha256.Sum256(ud.Bytes())
	meta := fmt.Sprintf("instance-id: iid-homelab-ops-appliance-%s\nlocal-hostname: %s\n",
		hex.EncodeToString(sum[:6]), Hostname)
	return Seed{UserData: ud.Bytes(), MetaData: []byte(meta), Skipped: skipped}, nil
}

// pinNote starts the comment line directly above services.web.image in
// deploy/compose.yaml that names what its digest was built from.
const pinNote = "# This pin is "

// PinImage returns compose with services.web.image set to image. image is
// <repo>@sha256:<hex>, or a bare sha256:<hex> for the repo compose already
// names, less any tag it carries; "" or the current reference returns
// compose unchanged. Otherwise it changes only that line, and drops the
// pinNote line above it, which describes the old digest.
func PinImage(compose []byte, image string) ([]byte, error) {
	cur, line, err := composeImage(compose)
	if err != nil {
		return nil, err
	}
	if image == "" {
		return compose, nil
	}
	if digestRe.MatchString(image) {
		repo, _, _ := strings.Cut(cur, "@")
		// A tag that named the old digest doesn't name this one.
		if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
			repo = repo[:i]
		}
		image = repo + "@" + image
	}
	if !imageRe.MatchString(image) {
		return nil, fmt.Errorf("image %q: want a reference pinned by digest, <repo>@sha256:<64 hex>, or a bare sha256:<64 hex>; the web image is never followed by tag", image)
	}
	if image == cur {
		return compose, nil
	}
	lines := strings.SplitAfter(string(compose), "\n")
	l := lines[line-1]
	i := strings.Index(l, cur)
	if i < 0 {
		return nil, fmt.Errorf("deploy/compose.yaml line %d: services.web.image isn't on one line", line)
	}
	lines[line-1] = l[:i] + image + l[i+len(cur):]
	if line >= 2 && strings.HasPrefix(strings.TrimSpace(lines[line-2]), pinNote) {
		lines = append(lines[:line-2], lines[line-1:]...)
	}
	return []byte(strings.Join(lines, "")), nil
}

// ComposeImage returns compose's services.web.image.
func ComposeImage(compose []byte) (string, error) {
	img, _, err := composeImage(compose)
	return img, err
}

func composeImage(compose []byte) (string, int, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(compose, &doc); err != nil {
		return "", 0, fmt.Errorf("deploy/compose.yaml: %w", err)
	}
	n := &doc
	for _, key := range []string{"services", "web", "image"} {
		if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
			n = n.Content[0]
		}
		n = mapValue(n, key)
		if n == nil {
			return "", 0, errors.New("deploy/compose.yaml has no services.web.image")
		}
	}
	if n.Kind != yaml.ScalarNode || n.Value == "" {
		return "", 0, errors.New("deploy/compose.yaml's services.web.image isn't a string")
	}
	return n.Value, n.Line, nil
}

// composeEnv returns compose's services.web.environment, a list of
// NAME=value: the environment the web app runs with on the appliance.
func composeEnv(compose []byte) (map[string]string, error) {
	var c struct {
		Services struct {
			Web struct {
				Environment []string `yaml:"environment"`
			} `yaml:"web"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(compose, &c); err != nil {
		return nil, fmt.Errorf("deploy/compose.yaml: %w", err)
	}
	env := map[string]string{}
	for _, kv := range c.Services.Web.Environment {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("deploy/compose.yaml: services.web.environment entry %q isn't NAME=value", kv)
		}
		env[k] = v
	}
	return env, nil
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// ParseAuthorizedKeys reads an authorized_keys file: one public key per line;
// blank lines and # comments are skipped. Options before the key type
// aren't supported.
func ParseAuthorizedKeys(data []byte) ([]string, error) {
	var keys []string
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := checkAuthorizedKey(line); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		keys = append(keys, line)
	}
	if len(keys) == 0 {
		return nil, errors.New("no keys")
	}
	return keys, nil
}

// checkAuthorizedKey holds one SSHAuthorizedKeys entry to what
// settings.json may carry into root's authorized_keys: exactly one bare
// public key, on one line, with no options before the key type.
func checkAuthorizedKey(k string) error {
	if strings.ContainsAny(k, "\r\n") {
		return errors.New("holds a line break; give one key per entry")
	}
	pub, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(k))
	if err != nil {
		return err
	}
	if len(options) > 0 {
		return fmt.Errorf("key options (%s) aren't supported; give the bare key", strings.Join(options, ","))
	}
	if pub == nil || len(bytes.TrimSpace(rest)) > 0 {
		return errors.New("want exactly one public key")
	}
	return nil
}

// Write writes the seed into dir as user-data and meta-data, mode 0600:
// user-data holds secrets/ unless rendered with NoSecrets. It refuses to
// overwrite either file unless force.
func Write(dir string, s Seed, force bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
	}{{"user-data", s.UserData}, {"meta-data", s.MetaData}}
	if !force {
		for _, f := range files {
			if _, err := os.Lstat(filepath.Join(dir, f.name)); err == nil {
				return fmt.Errorf("%s already exists (pass --force to overwrite)", filepath.Join(dir, f.name))
			}
		}
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, f.data, 0o600); err != nil {
			return err
		}
		// WriteFile keeps an existing file's mode.
		if err := os.Chmod(p, 0o600); err != nil {
			return err
		}
	}
	return nil
}

type cloudConfig struct {
	Bootcmd    []command   `yaml:"bootcmd,omitempty"`
	WriteFiles []writeFile `yaml:"write_files"`
}

// command is one bootcmd entry: an argv, run without a shell. It marshals
// in flow style, one command per line.
type command []string

// MarshalYAML implements yaml.Marshaler.
func (c command) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, a := range c {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: a})
	}
	return n, nil
}

type writeFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner"`
	Permissions string `yaml:"permissions"`
	Encoding    string `yaml:"encoding,omitempty"`
	Content     string `yaml:"content"`
}

// newWriteFile carries text as-is, and anything else base64-encoded.
func newWriteFile(target string, mode fs.FileMode, content []byte) writeFile {
	wf := writeFile{Path: target, Owner: "root:root", Permissions: fmt.Sprintf("%04o", mode.Perm())}
	if utf8.Valid(content) && !bytes.ContainsRune(content, 0) {
		wf.Content = string(content)
	} else {
		wf.Encoding = "b64"
		wf.Content = base64.StdEncoding.EncodeToString(content)
	}
	return wf
}

type entry struct {
	rel     string // slash-separated, relative to the config dir; "." is the dir itself
	dir     bool
	mode    fs.FileMode
	content []byte
}

// readConfigDir reads every directory and file under dir, parents first,
// less dot entries and editor backups, which it returns as skipped, and
// less secrets/ with noSecrets: the entries are what the seed ships. It
// checks those entries as the appliance's web app will load them, with env
// as its environment (checkShipped), and refuses what the appliance
// couldn't start from.
func readConfigDir(dir string, noSecrets bool, env map[string]string) ([]entry, []string, error) {
	if dir == "" {
		return nil, nil, errors.New("no config directory given")
	}
	// WalkDir doesn't follow a symlinked root: resolve it first.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("%s is not a directory", dir)
	}
	var entries []entry
	var skipped []string
	var problems []error
	sawSecrets := false
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if noSecrets && inSecrets(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if name := d.Name(); rel != "." && (strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~")) {
			skipped = append(skipped, rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == deployconfig.SecretsDir {
			sawSecrets = true
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode := fi.Mode().Perm()
		switch {
		case d.IsDir():
			entries = append(entries, entry{rel: rel, dir: true, mode: mode})
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p) //nolint:gosec // G304: the path is under the operator-supplied --config-dir
			if err != nil {
				return err
			}
			entries = append(entries, entry{rel: rel, mode: mode, content: b})
		default:
			problems = append(problems, fmt.Errorf("%s: not a regular file or directory (%s); the seed carries only those", rel, fi.Mode().Type()))
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if !noSecrets && !sawSecrets {
		problems = append(problems, fmt.Errorf("no %s/ in %s: the appliance's web service waits for it; add %s, or pass --no-secrets and copy it in after boot", deployconfig.SecretsDir, dir, deployconfig.WireGuardKeyFile))
	}
	// WalkDir visits in lexical order, which already puts a directory before
	// its contents; sort stably by depth anyway so bootcmd's mkdirs, and
	// stage's, never depend on that.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].dir && !entries[j].dir ||
			entries[i].dir == entries[j].dir && depth(entries[i].rel) < depth(entries[j].rel)
	})
	problems = append(problems, checkShipped(entries, skipped, noSecrets, env)...)
	if len(problems) > 0 {
		return nil, nil, fmt.Errorf("config directory %s:\n%w", dir, errors.Join(problems...))
	}
	return entries, skipped, nil
}

func inSecrets(rel string) bool {
	return rel == deployconfig.SecretsDir || strings.HasPrefix(rel, deployconfig.SecretsDir+"/")
}

// checkShipped runs the web app's own startup checks (deployconfig.LoadDir,
// with env as the environment) on entries, staged as the appliance will
// have them, so a file the seed leaves out is neither checked nor counted
// on. On top it holds the path settings to files the seed carries, and the
// WireGuard settings to the appliance's.
func checkShipped(entries []entry, skipped []string, noSecrets bool, env map[string]string) []error {
	staged, cleanup, err := stage(entries)
	defer cleanup()
	if err != nil {
		return []error{fmt.Errorf("staging the files to check them: %w", err)}
	}
	cfg, err := deployconfig.LoadDir(staged, env)
	if err != nil {
		return []error{err}
	}
	var problems []error
	files := map[string]bool{}
	for _, e := range entries {
		if !e.dir {
			files[e.rel] = true
		}
	}
	for _, s := range []struct{ name, path string }{
		{"NETWORK_ADDRESSING_PATH", cfg.NetworkAddressingPath},
		{"BASE_IMAGE_PATH", cfg.BaseImagePath},
		{"CLIENT_CERT_PATH", cfg.ClientCertPath},
	} {
		if s.path == "" {
			continue
		}
		if err := shippedFile(s.path, staged, files, skipped, noSecrets); err != nil {
			problems = append(problems, fmt.Errorf("%s=%s: %w", s.name, s.path, err))
		}
	}
	if cfg.WireGuardEndpoint == "" {
		problems = append(problems, errors.New("WIREGUARD_ENDPOINT: unset, but the appliance serves the API only through the WireGuard tunnel (deploy/compose.yaml publishes no API listener); set it in web.env"))
	}
	if cfg.WireGuardPort != WireGuardPort {
		problems = append(problems, fmt.Errorf("%s: WIREGUARD_PORT=%d, but the appliance publishes and opens only %d/udp (deploy/compose.yaml); leave it unset", deployconfig.WebEnvFile, cfg.WireGuardPort, WireGuardPort))
	}
	return problems
}

// shippedFile says why p, a path setting's value, isn't a file the seed
// carries, or returns nil. The appliance's web container sees the config
// directory at ConfigDir, and LoadDir read it at staged. With noSecrets, a
// file under secrets/ counts: the web app starts only once secrets/ is
// copied in.
func shippedFile(p, staged string, files map[string]bool, skipped []string, noSecrets bool) error {
	var rel string
	if r, ok := strings.CutPrefix(p, staged+string(filepath.Separator)); ok {
		rel = filepath.ToSlash(r)
	} else if r, ok := strings.CutPrefix(path.Clean(p), ConfigDir+"/"); ok && path.IsAbs(p) {
		rel = r
	} else {
		return fmt.Errorf("not under %s, the only directory of operator files the appliance's web container mounts; put the file in --config-dir and name it %s/<file>", ConfigDir, ConfigDir)
	}
	if files[rel] || noSecrets && inSecrets(rel) {
		return nil
	}
	for _, s := range skipped {
		if rel == s || strings.HasPrefix(rel, s+"/") {
			return fmt.Errorf("%s is left out of the seed, as dot entries and editor backups are; rename it", rel)
		}
	}
	return fmt.Errorf("the seed carries no file %s: --config-dir has none", rel)
}

// stage writes entries, parents first, into a new private temporary
// directory with their modes, and returns the config directory's path in
// it and a cleanup that removes the lot (secrets/ included).
func stage(entries []entry) (string, func(), error) {
	parent, err := os.MkdirTemp("", "appliance-seed-check-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		// A staged directory's mode may lack the owner's write or search bit.
		_ = filepath.WalkDir(parent, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700) //nolint:gosec // G302: a directory, under our own private temporary one
			}
			return nil
		})
		_ = os.RemoveAll(parent)
	}
	root := filepath.Join(parent, "web")
	for _, e := range entries {
		p := filepath.Join(root, filepath.FromSlash(e.rel))
		if e.dir {
			err = os.Mkdir(p, 0o700)
		} else if err = os.WriteFile(p, e.content, 0o600); err == nil {
			err = os.Chmod(p, e.mode)
		}
		if err != nil {
			return "", cleanup, err
		}
	}
	// Directory modes last, deepest first, so none stops a write below it.
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; e.dir {
			if err := os.Chmod(filepath.Join(root, filepath.FromSlash(e.rel)), e.mode); err != nil {
				return "", cleanup, err
			}
		}
	}
	return root, cleanup, nil
}

func depth(rel string) int {
	if rel == "." {
		return 0
	}
	return strings.Count(rel, "/") + 1
}
