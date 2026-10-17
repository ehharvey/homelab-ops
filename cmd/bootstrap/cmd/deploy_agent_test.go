package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision/nodeprovisiontest"
)

const deployAgentAppFixture = `
kind: App
name: agent
type: agent
replicas: per-node
image:
  server: https://ghcr.io
  protocol: oci
  alias: ehharvey/homelab-ops/agent:latest
`

const deployAgentSecondInstance = `
kind: Instance
name: node1
mac: aa:bb:cc:dd:ee:01
network: home-lan
static_ip: 192.168.1.202
disk: single
nic: single
applications: [incus]
`

// deployAgentEnv is a temp dir holding fleet files and a break-glass
// cert/key pair.
type deployAgentEnv struct {
	dir, certPath, keyPath string
}

func newDeployAgentEnv(t *testing.T) deployAgentEnv {
	t.Helper()
	dir := t.TempDir()
	pair, err := cert.Generate(cert.Options{CommonName: "bootstrap@operator", ValidityDays: 1})
	if err != nil {
		t.Fatalf("cert.Generate: %v", err)
	}
	certPath, keyPath, err := cert.Write(filepath.Join(dir, "cert"), pair, false)
	if err != nil {
		t.Fatalf("cert.Write: %v", err)
	}
	return deployAgentEnv{dir: dir, certPath: certPath, keyPath: keyPath}
}

func (e deployAgentEnv) file(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(e.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// execDeployAgent runs `bootstrap deploy-agent args...`, first resetting the
// command's package-level flag variables, which persist across Execute
// calls in one test binary.
func execDeployAgent(t *testing.T, args ...string) (string, error) {
	t.Helper()
	deployAgentFiles = nil
	deployAgentInstance = ""
	deployAgentCertPath = "./bootstrap-output/cert/client.crt"
	deployAgentKeyPath = "./bootstrap-output/cert/client.key"
	deployAgentAddr = ""
	deployAgentRepoURL = ""
	deployAgentRepoRef = "main"
	deployAgentTimeout = 10 * time.Minute

	rootCmd.SetArgs(append([]string{"deploy-agent"}, args...))
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	err := rootCmd.Execute()
	return out.String(), err
}

func TestDeployAgentCreatesThenReportsAlreadyDeployed(t *testing.T) {
	env := newDeployAgentEnv(t)
	// Split across two files, as in examples/fleet/.
	fleet := env.file(t, "fleet.yaml", renderSeedFixture)
	apps := env.file(t, "apps.yaml", deployAgentAppFixture)
	node := nodeprovisiontest.New(t, "unrelated")
	args := []string{
		"--file", fleet, "--file", apps,
		"--cert", env.certPath, "--key", env.keyPath,
		"--addr", node.Addr(),
		"--config-repo-url", "https://example.test/fleet.git",
	}

	out, err := execDeployAgent(t, args...)
	if err != nil {
		t.Fatalf("deploy-agent: %v\n%s", err, out)
	}
	if !strings.Contains(out, "deployed agent-node0-g0 on node0") {
		t.Errorf("output %q does not report the deploy", out)
	}
	created := node.Created()
	if len(created) != 1 || created[0].Name != "agent-node0-g0" {
		t.Fatalf("created = %+v, want one agent-node0-g0", created)
	}
	if got := created[0].Config["environment.CONFIG_REPO_REF"]; got != "main" {
		t.Errorf("CONFIG_REPO_REF = %q, want the default main", got)
	}

	out, err = execDeployAgent(t, args...)
	if err != nil {
		t.Fatalf("second deploy-agent: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already deployed: agent-node0-g0") {
		t.Errorf("second run output %q does not report already deployed", out)
	}
	if n := len(node.Created()); n != 1 {
		t.Errorf("created %d instances after the rerun, want still 1", n)
	}
	if got := node.Revoked(); len(got) != 0 {
		t.Errorf("revoked %v; the break-glass cert must never be revoked", got)
	}
}

func TestDeployAgentSelectsInstance(t *testing.T) {
	env := newDeployAgentEnv(t)
	fleet := env.file(t, "fleet.yaml", renderSeedFixture+"---\n"+deployAgentSecondInstance+"---\n"+deployAgentAppFixture)
	node := nodeprovisiontest.New(t)

	out, err := execDeployAgent(t, "--file", fleet, "--instance", "node1",
		"--cert", env.certPath, "--key", env.keyPath, "--addr", node.Addr(),
		"--config-repo-url", "https://example.test/fleet.git")
	if err != nil {
		t.Fatalf("deploy-agent: %v\n%s", err, out)
	}
	if created := node.Created(); len(created) != 1 || created[0].Name != "agent-node1-g0" {
		t.Errorf("created = %+v, want one agent-node1-g0", created)
	}
}

func TestDeployAgentRejects(t *testing.T) {
	env := newDeployAgentEnv(t)
	withApp := env.file(t, "with-app.yaml", renderSeedFixture+"---\n"+deployAgentAppFixture)
	noApp := env.file(t, "no-app.yaml", renderSeedFixture)
	twoInstances := env.file(t, "two.yaml", renderSeedFixture+"---\n"+deployAgentSecondInstance+"---\n"+deployAgentAppFixture)
	node := nodeprovisiontest.New(t)
	creds := []string{"--cert", env.certPath, "--key", env.keyPath}
	repo := []string{"--config-repo-url", "https://example.test/fleet.git"}

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no file", args: append(append([]string{"--addr", node.Addr()}, creds...), repo...), wantErr: "--file is required"},
		{name: "no addr", args: append(append([]string{"--file", withApp}, creds...), repo...), wantErr: "--addr is required"},
		{name: "no repo url", args: append([]string{"--file", withApp, "--addr", node.Addr()}, creds...), wantErr: "--config-repo-url is required"},
		{name: "no agent app", args: append(append([]string{"--file", noApp, "--addr", node.Addr()}, creds...), repo...), wantErr: "no agent App declared"},
		{name: "ambiguous instance", args: append(append([]string{"--file", twoInstances, "--addr", node.Addr()}, creds...), repo...), wantErr: "pass --instance"},
		{name: "unknown instance", args: append(append([]string{"--file", withApp, "--instance", "nope", "--addr", node.Addr()}, creds...), repo...), wantErr: `no Instance named "nope"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execDeployAgent(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("deploy-agent error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
	if n := len(node.Created()); n != 0 {
		t.Errorf("created %d instances across rejected runs, want none", n)
	}
}
