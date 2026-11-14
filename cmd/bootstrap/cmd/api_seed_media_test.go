package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/hostos"
)

// The seed the fake web app hands out, as POST /instances/{name}/seed
// answers it.
const fakeSeedBody = `{"install_yaml":"force_install: false\n","network_yaml":"private_key: secret\n","applications_yaml":"applications: []\n","incus_yaml":"apply_defaults: true\n"}`

// fakeSeedAPI serves GET /instances (instances, a JSON list) and node0's
// seed, counting seed requests.
func fakeSeedAPI(t *testing.T, instances string) *int {
	t.Helper()
	var seeds int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /instances", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, instances) })
	mux.HandleFunc("POST /instances/node0/seed", func(w http.ResponseWriter, _ *http.Request) {
		seeds++
		_, _ = io.WriteString(w, fakeSeedBody)
	})
	mux.HandleFunc("GET /instances/node0/image", func(http.ResponseWriter, *http.Request) {
		t.Error("a local build asked the web app for its image")
	})
	fakeAPI(t, mux)
	t.Cleanup(resetMediaFlags)
	resetMediaFlags()
	return &seeds
}

// resetMediaFlags is resetAPIFlags for the client-side media flags, cobra's
// Changed marks included (runAPIImage reads --flasher-tool-bin's).
func resetMediaFlags() {
	apiSeedMediaOutput = ""
	apiImageBaseImage = ""
	apiImageFlasherToolBin = "flasher-tool"
	for _, name := range []string{"base-image", "flasher-tool-bin"} {
		apiImageCmd.Flags().Lookup(name).Changed = false
	}
}

func TestAPISeedMediaWritesSeedDataISO(t *testing.T) {
	seeds := fakeSeedAPI(t, `[{"Name":"node0"}]`)
	path := filepath.Join(t.TempDir(), "seed.iso")
	out, stderr, err := runAPI("api", "seed-media", "node0", "--output", path)
	if err != nil {
		t.Fatalf("seed-media: %v", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want the media only in the file", out)
	}
	if *seeds != 1 {
		t.Errorf("fetched the seed %d times, want 1", *seeds)
	}
	img, err := os.ReadFile(path) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimRight(string(img[16*2048+40:16*2048+72]), " "); got != "SEED_DATA" {
		t.Errorf("volume ID = %q, want SEED_DATA", got)
	}
	for _, s := range []string{"INSTALL.YAML;1", "NETWORK.YAML;1", "APPLICATIONS.YAML;1", "INCUS.YAML;1", "private_key: secret\n"} {
		if !bytes.Contains(img, []byte(s)) {
			t.Errorf("the media lacks %q", s)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("stat %s: %v, mode %v; want mode 600", path, err, info.Mode().Perm())
	}
	if !strings.Contains(stderr, "Remove the media once the install finishes") {
		t.Errorf("stderr = %q, want the remove-after-install reminder", stderr)
	}

	// To stdout: the same image, and the reminder still on stderr.
	resetAPIFlags(t)
	resetMediaFlags()
	out, stderr, err = runAPI("api", "seed-media", "node0", "-o", "-")
	if err != nil || out != string(img) {
		t.Errorf("seed-media -o -: err %v, %d bytes on stdout; want the %d-byte image", err, len(out), len(img))
	}
	if !strings.Contains(stderr, "Remove the media") {
		t.Errorf("stderr = %q, want the reminder", stderr)
	}
}

// TestAPISeedMediaRefusesBeforeTheSeed: an instance whose os has no backend,
// or one the web app doesn't know, fails naming it, before the seed route
// mints a bootstrap credential, and writes nothing.
func TestAPISeedMediaRefusesBeforeTheSeed(t *testing.T) {
	for _, tc := range []struct{ instances, want string }{
		{`[{"Name":"node0","OS":"talos"}]`, `no host OS backend for "talos"`},
		{`[{"Name":"node1"}]`, `instance "node0" not found`},
	} {
		seeds := fakeSeedAPI(t, tc.instances)
		path := filepath.Join(t.TempDir(), "seed.iso")
		_, _, err := runAPI("api", "seed-media", "node0", "--output", path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("instances %s: err = %v, want %q", tc.instances, err, tc.want)
		}
		if *seeds != 0 {
			t.Errorf("instances %s: fetched the seed %d times, want 0", tc.instances, *seeds)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("instances %s: a refusal left %s behind (stat: %v)", tc.instances, path, err)
		}
	}
}

// TestFetchSeedChecksTheBackendBeforeTheSeed: a backend fetchSeed's check
// refuses (seed-media passes hostos.CheckSeedMedia, which refuses an OS with
// no seed media) fails naming the instance, with no POST .../seed, so no
// bootstrap credential is minted.
func TestFetchSeedChecksTheBackendBeforeTheSeed(t *testing.T) {
	seeds := fakeSeedAPI(t, `[{"Name":"node0"}]`)
	c, done, err := apiConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	refuse := func(b hostos.Backend) error { return hostos.NoSeedMedia(b.OS()) }
	_, _, err = fetchSeed(context.Background(), c, "node0", refuse)
	if !errors.Is(err, hostos.ErrNoSeedMedia) || !strings.Contains(err.Error(), `instance "node0"`) {
		t.Errorf("fetchSeed error = %v, want ErrNoSeedMedia naming node0", err)
	}
	if *seeds != 0 {
		t.Errorf("fetched the seed %d times, want 0", *seeds)
	}
}

// fakeFlasherTool writes a flasher-tool stand-in that copies the seed tar
// it's given to keptTar, and its path to keptTar.path, and marks the image,
// or fails when fail is set.
func fakeFlasherTool(t *testing.T, dir, keptTar string, fail bool) string {
	t.Helper()
	tool := filepath.Join(dir, "flasher-tool")
	script := "#!/bin/sh\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --image) image=\"$2\" ;;\n" +
		"    --seed) cp \"$2\" " + keptTar + " && echo \"$2\" > " + keptTar + ".path ;;\n" +
		"  esac\n" +
		"  shift\n" +
		"done\n"
	if fail {
		script += "echo flasher-tool failed >&2\nexit 1\n"
	} else {
		script += "printf _seeded >> \"$image\"\n"
	}
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, intentionally executable
		t.Fatal(err)
	}
	return tool
}

func TestAPIImageBuildsLocallyFromTheSeed(t *testing.T) {
	seeds := fakeSeedAPI(t, `[{"Name":"node0","OS":"incusos"}]`)
	dir := t.TempDir()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	keptTar := filepath.Join(dir, "kept.tar")
	tool := fakeFlasherTool(t, dir, keptTar, false)
	base := filepath.Join(dir, "base.img")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	out := filepath.Join(dir, "node0.img")

	_, stderr, err := runAPI("api", "image", "node0", "--output", out, "--base-image", base, "--flasher-tool-bin", tool)
	if err != nil {
		t.Fatalf("image --base-image: %v\n%s", err, stderr)
	}
	if *seeds != 1 {
		t.Errorf("fetched the seed %d times, want 1", *seeds)
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "base_seeded" { //nolint:gosec // test temp file
		t.Errorf("output = %q, %v; want the base image plus the tool's mark", got, err)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("output mode: %v, %v; want 600", info.Mode().Perm(), err)
	}
	if got, _ := os.ReadFile(base); string(got) != "base" { //nolint:gosec // test temp file
		t.Errorf("base image = %q, want it unmodified", got)
	}

	// The tool got the downloaded seed, file for file.
	f, err := os.Open(keptTar) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("the tool got no seed tar: %v", err)
	}
	defer f.Close() //nolint:errcheck // test read
	tr := tar.NewReader(f)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		got[hdr.Name] = string(data)
	}
	if got["network.yaml"] != "private_key: secret\n" || len(got) != 4 {
		t.Errorf("seed tar = %v, want the four downloaded files", got)
	}
	// The tar sat in the private seed directory, not in TMPDIR itself.
	tarPath, _ := os.ReadFile(keptTar + ".path") //nolint:gosec // test temp file
	if d := filepath.Dir(strings.TrimSpace(string(tarPath))); filepath.Dir(d) != tmp || !strings.HasPrefix(filepath.Base(d), "bootstrap-seed-") {
		t.Errorf("seed tar was at %q, want it inside the private bootstrap-seed-* directory", tarPath)
	}

	// Nothing of the seed is left in the temp dir, and no partial file
	// beside --output.
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left in TMPDIR: %v", left)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".node0.img.partial-*")); len(matches) != 0 {
		t.Errorf("partial files left: %v", matches)
	}
}

func TestAPIImageLocalBuildFailureKeepsAnExistingFile(t *testing.T) {
	fakeSeedAPI(t, `[{"Name":"node0"}]`)
	dir := t.TempDir()
	tool := fakeFlasherTool(t, dir, filepath.Join(dir, "kept.tar"), true)
	base := filepath.Join(dir, "base.img")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	out := filepath.Join(dir, "node0.img")
	if err := os.WriteFile(out, []byte("older image"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runAPI("api", "image", "node0", "--output", out, "--base-image", base, "--flasher-tool-bin", tool)
	if err == nil || !strings.Contains(err.Error(), "build node0's image locally") {
		t.Fatalf("err = %v, want the local build's failure", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "older image" { //nolint:gosec // test temp file
		t.Errorf("output = %q, want the older file untouched", got)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".node0.img.partial-*")); len(matches) != 0 {
		t.Errorf("partial files left: %v", matches)
	}
}

// TestAPIImageLocalBuildWritesThroughASymlink: a symlinked --output keeps
// its link, and the image lands at the link's target, as a streamed one does.
func TestAPIImageLocalBuildWritesThroughASymlink(t *testing.T) {
	fakeSeedAPI(t, `[{"Name":"node0"}]`)
	dir := t.TempDir()
	tool := fakeFlasherTool(t, dir, filepath.Join(dir, "kept.tar"), false)
	base := filepath.Join(dir, "base.img")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	media := t.TempDir()
	target := filepath.Join(media, "node0.img")
	if err := os.WriteFile(target, []byte("older image"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "node0.img")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runAPI("api", "image", "node0", "--output", link, "--base-image", base, "--flasher-tool-bin", tool); err != nil {
		t.Fatalf("image --base-image: %v\n%s", err, stderr)
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Errorf("--output link = %q, %v; want it still pointing at %s", got, err, target)
	}
	if got, _ := os.ReadFile(target); string(got) != "base_seeded" { //nolint:gosec // test temp file
		t.Errorf("link target = %q, want the built image", got)
	}
}

func TestAPIImageLocalFlagsChecked(t *testing.T) {
	connects := fakeAPI(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the request was sent")
	}))
	t.Cleanup(resetMediaFlags)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.img")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--output", "-", "--base-image", base}, "a local build (--base-image) writes a file"},
		{[]string{"--output", filepath.Join(dir, "x.img"), "--base-image", filepath.Join(dir, "missing.img")}, "--base-image"},
		{[]string{"--output", os.DevNull, "--base-image", base}, "isn't a regular file"},
		{[]string{"--output", filepath.Join(dir, "x.img"), "--flasher-tool-bin", "/bin/true"}, "--flasher-tool-bin needs --base-image"},
		{[]string{"--output", filepath.Join(dir, "x.img"), "--base-image", base, "--flasher-tool-bin", "definitely-not-a-real-flasher-tool"}, `--flasher-tool-bin "definitely-not-a-real-flasher-tool"`},
	} {
		resetAPIFlags(t)
		resetMediaFlags()
		_, _, err := runAPI(append([]string{"api", "image", "node0"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("image %v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	if *connects != 0 {
		t.Errorf("connected %d times, want none", *connects)
	}
}

// TestAPISeedMediaReplacesAnExistingFile0600: an existing --output, here
// left world-readable by an earlier run, ends mode 0600 holding the new
// media, since it's built beside it and renamed over it; a symlinked one
// replaces its target and keeps the link; no partial file is left.
func TestAPISeedMediaReplacesAnExistingFile0600(t *testing.T) {
	fakeSeedAPI(t, `[{"Name":"node0"}]`)
	dir := t.TempDir()
	target := filepath.Join(dir, "seed.iso")
	if err := os.WriteFile(target, []byte("older media"), 0o644); err != nil { //nolint:gosec // G306: the world-readable file under test
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "seed.iso")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{target, link} {
		if err := os.Chmod(target, 0o644); err != nil { //nolint:gosec // G302: the world-readable file under test
			t.Fatal(err)
		}
		resetAPIFlags(t)
		resetMediaFlags()
		if _, stderr, err := runAPI("api", "seed-media", "node0", "--output", out); err != nil {
			t.Fatalf("seed-media -o %s: %v\n%s", out, err, stderr)
		}
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("-o %s: %s is %v, %v; want a regular file, mode 600", out, target, info.Mode(), err)
		}
		if img, _ := os.ReadFile(target); !bytes.Contains(img, []byte("private_key: secret\n")) { //nolint:gosec // test temp file
			t.Errorf("-o %s: %s doesn't hold the new media", out, target)
		}
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Errorf("--output link = %q, %v; want it still pointing at %s", got, err, target)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".seed.iso.partial-*")); len(matches) != 0 {
		t.Errorf("partial files left: %v", matches)
	}
}

// TestMediaOutputDirMustTakeANewFile: an --output file that exists and is
// writable, in a directory that won't take a new file, is refused before
// POST .../seed, for seed-media and the local image build alike: both
// rename a file into that directory, and a refusal after the seed would
// have minted a credential for nothing.
func TestMediaOutputDirMustTakeANewFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := t.TempDir()
	tool := fakeFlasherTool(t, dir, filepath.Join(dir, "kept.tar"), false)
	base := filepath.Join(dir, "base.img")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	media := t.TempDir()
	out := filepath.Join(media, "node0.img")
	if err := os.WriteFile(out, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(media, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(media, 0o700) }) //nolint:gosec // G302: restore the temp dir so it can be removed
	for _, args := range [][]string{
		{"api", "seed-media", "node0", "--output", out},
		{"api", "image", "node0", "--output", out, "--base-image", base, "--flasher-tool-bin", tool},
	} {
		seeds := fakeSeedAPI(t, `[{"Name":"node0"}]`)
		_, _, err := runAPI(args...)
		if err == nil || !strings.Contains(err.Error(), "open --output") {
			t.Errorf("%v: err = %v, want it refused opening --output", args, err)
		}
		if *seeds != 0 {
			t.Errorf("%v: fetched the seed %d times, want 0", args, *seeds)
		}
		if got, _ := os.ReadFile(out); string(got) != "older" { //nolint:gosec // test temp file
			t.Errorf("%v: --output = %q, want it untouched", args, got)
		}
	}
}

// TestAPIImageLocalRefusesTheBaseImageAsOutput: an --output that is
// --base-image, by its own path, through a symlink, or as a hard link, is
// refused before any request, and the base is left as it was: the build
// would rename the seeded copy over it.
func TestAPIImageLocalRefusesTheBaseImageAsOutput(t *testing.T) {
	connects := fakeAPI(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the request was sent")
	}))
	t.Cleanup(resetMediaFlags)
	dir := t.TempDir()
	tool := fakeFlasherTool(t, dir, filepath.Join(dir, "kept.tar"), false)
	base := filepath.Join(dir, "base.img")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "node0.img")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(dir, "hard.img")
	if err := os.Link(base, hard); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{base, link, hard} {
		resetAPIFlags(t)
		resetMediaFlags()
		_, _, err := runAPI("api", "image", "node0", "--output", out, "--base-image", base, "--flasher-tool-bin", tool)
		if err == nil || !strings.Contains(err.Error(), "is --base-image") {
			t.Errorf("--output %s: err = %v, want it refused as the base image", out, err)
		}
	}
	if got, _ := os.ReadFile(base); string(got) != "base" { //nolint:gosec // test temp file
		t.Errorf("base image = %q, want it unmodified", got)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".*.partial-*")); len(matches) != 0 {
		t.Errorf("partial files left: %v", matches)
	}
	if *connects != 0 {
		t.Errorf("connected %d times, want none", *connects)
	}
}

// TestFetchSeedReadsTheWholeInstance: fetchSeed reads GET /instances as the
// server encodes it, a []config.Instance, so a fully populated instance
// decodes and selects its backend.
func TestFetchSeedReadsTheWholeInstance(t *testing.T) {
	inst := config.Instance{
		Name: "node0", MAC: "52:54:00:00:00:01", Network: "lan",
		StaticIP: netip.MustParseAddr("10.0.0.5"), Disk: "/dev/sda", NIC: "enp1s0",
		Applications: []string{"incus"}, OS: config.OSIncusOS, Runtime: config.RuntimeIncus,
	}
	list, err := json.Marshal([]config.Instance{{Name: "other"}, inst})
	if err != nil {
		t.Fatal(err)
	}
	seeds := fakeSeedAPI(t, string(list))
	c, done, err := apiConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var got hostos.Backend
	backend, files, err := fetchSeed(context.Background(), c, "node0", func(b hostos.Backend) error { got = b; return nil })
	if err != nil {
		t.Fatalf("fetchSeed: %v", err)
	}
	if backend.OS() != config.OSIncusOS || got != backend {
		t.Errorf("backend = %v (checked %v), want IncusOS", backend.OS(), got)
	}
	if len(files) != 4 || *seeds != 1 {
		t.Errorf("got %d files from %d seed requests, want 4 from 1", len(files), *seeds)
	}
}
