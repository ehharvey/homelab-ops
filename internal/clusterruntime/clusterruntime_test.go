package clusterruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision/nodeprovisiontest"
)

// TestRuntimesMatchConfig keeps the registry and config.Validate in step: a
// runtime config.Validate accepts must be registered, and a registered
// runtime must be one config.Validate accepts.
func TestRuntimesMatchConfig(t *testing.T) {
	for _, name := range config.Runtimes() {
		_, err := For(name)
		if implemented := config.RuntimeImplemented(name); implemented != (err == nil) {
			t.Errorf("runtime %q: config.RuntimeImplemented = %v, but For error = %v", name, implemented, err)
		}
	}
	for name, r := range runtimes {
		if r.Name() != name {
			t.Errorf("runtimes[%q].Name() = %q", name, r.Name())
		}
		if !config.RuntimeImplemented(name) {
			t.Errorf("runtime %q is registered but config.RuntimeImplemented(%q) is false", name, name)
		}
	}
}

func TestForRejectsUnregisteredRuntime(t *testing.T) {
	for _, name := range []string{config.RuntimeKubernetes, "", "nomad"} {
		if r, err := For(name); !errors.Is(err, ErrNoRuntime) {
			t.Errorf("For(%q) = %v, %v; want ErrNoRuntime", name, r, err)
		}
	}
}

func TestForInstance(t *testing.T) {
	r, err := ForInstance(config.Instance{Name: "node0"})
	if err != nil || r.Name() != config.RuntimeIncus {
		t.Errorf("ForInstance(no runtime) = %v, %v; want %q", r, err, config.RuntimeIncus)
	}
	if r, err := ForInstance(config.Instance{Name: "node0", Runtime: config.RuntimeKubernetes}); err == nil {
		t.Errorf("ForInstance(runtime kubernetes) = %v, want an error", r.Name())
	}
}

func agentApp() config.App {
	return config.App{
		Name:     "agent",
		Type:     agentdeploy.AppType,
		Replicas: config.Replicas{PerNode: true},
		Image:    config.ImageRef{Server: "https://ghcr.io", Protocol: "oci", Alias: "ehharvey/homelab-ops/agent:latest"},
	}
}

// bootstrapInput is a Bootstrap of node0 against node, counting how often
// its Credential is asked for.
func bootstrapInput(t *testing.T, node *nodeprovisiontest.Server, oneShot bool, credCalls *int) BootstrapInput {
	t.Helper()
	pair, err := cert.Generate(cert.Options{CommonName: "bootstrap@node0", ValidityDays: 1})
	if err != nil {
		t.Fatalf("cert.Generate: %v", err)
	}
	return BootstrapInput{
		Apps:     []config.App{agentApp()},
		Instance: config.Instance{Name: "node0"},
		Repo:     agentdeploy.Repo{URL: "https://example.test/fleet.git", Ref: "main"},
		Dial:     node.Dial,
		Addr:     node.Addr(),
		Credential: func(context.Context) ([]byte, []byte, error) {
			*credCalls++
			return pair.CertPEM, pair.KeyPEM, nil
		},
		OneShot: oneShot,
	}
}

func TestIncusBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name        string
		existing    []string
		oneShot     bool
		wantAgent   string
		wantExisted bool
		wantCreated int
		wantRevoked int
	}{
		{name: "creates, break-glass", wantAgent: "agent-node0-g0", wantCreated: 1},
		{name: "creates, one-shot", oneShot: true, wantAgent: "agent-node0-g0", wantCreated: 1, wantRevoked: 1},
		{name: "already there, break-glass", existing: []string{"agent-node0-g2"}, wantAgent: "agent-node0-g2", wantExisted: true},
		{name: "already there, one-shot", existing: []string{"agent-node0-g2"}, oneShot: true, wantAgent: "agent-node0-g2", wantExisted: true, wantRevoked: 1},
		{name: "another node's agent", existing: []string{"agent-node1-g0"}, wantAgent: "agent-node0-g0", wantCreated: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := nodeprovisiontest.New(t, tc.existing...)
			var calls int
			res, err := Incus.Bootstrap(context.Background(), bootstrapInput(t, node, tc.oneShot, &calls))
			if err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			if res.Agent != tc.wantAgent || res.AlreadyDeployed != tc.wantExisted || res.RevokeErr != nil {
				t.Errorf("Bootstrap = %+v, want agent %s, already deployed %v", res, tc.wantAgent, tc.wantExisted)
			}
			if n := len(node.Created()); n != tc.wantCreated {
				t.Errorf("created %d instances, want %d", n, tc.wantCreated)
			}
			if n := len(node.Revoked()); n != tc.wantRevoked {
				t.Errorf("revoked %d certs, want %d", n, tc.wantRevoked)
			}
			if calls != 1 {
				t.Errorf("Credential called %d times, want 1", calls)
			}
		})
	}
}

// TestIncusBootstrapNoAgentApp: a fleet with no agent App fails before its
// credential is read, so the web route mints none for it.
func TestIncusBootstrapNoAgentApp(t *testing.T) {
	node := nodeprovisiontest.New(t)
	var calls int
	in := bootstrapInput(t, node, true, &calls)
	in.Apps = nil
	if _, err := Incus.Bootstrap(context.Background(), in); !errors.Is(err, ErrNoAgentApp) {
		t.Fatalf("Bootstrap error = %v, want ErrNoAgentApp", err)
	}
	if calls != 0 {
		t.Errorf("Credential called %d times, want 0", calls)
	}
}

func TestIncusBootstrapErrors(t *testing.T) {
	credErr := errors.New("db down")
	for _, tc := range []struct {
		name     string
		setup    func(*BootstrapInput, *nodeprovisiontest.Server)
		wantNode bool
		wantIs   error
	}{
		{name: "credential", setup: func(in *BootstrapInput, _ *nodeprovisiontest.Server) {
			in.Credential = func(context.Context) ([]byte, []byte, error) { return nil, nil, credErr }
		}, wantIs: credErr},
		{name: "create fails", setup: func(_ *BootstrapInput, n *nodeprovisiontest.Server) { n.CreateErr = true }, wantNode: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := nodeprovisiontest.New(t)
			var calls int
			in := bootstrapInput(t, node, false, &calls)
			tc.setup(&in, node)
			_, err := Incus.Bootstrap(context.Background(), in)
			if err == nil || errors.Is(err, ErrNode) != tc.wantNode {
				t.Errorf("Bootstrap error = %v, want one that is ErrNode: %v", err, tc.wantNode)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("Bootstrap error = %v, want %v", err, tc.wantIs)
			}
		})
	}
}

// TestIncusBootstrapSpentCredential: a node that no longer trusts the
// credential is a node error that is also nodeprovision.ErrUntrusted, which
// the web route maps to 409.
func TestIncusBootstrapSpentCredential(t *testing.T) {
	node := nodeprovisiontest.New(t)
	var calls int
	in := bootstrapInput(t, node, true, &calls)
	if _, err := Incus.Bootstrap(context.Background(), in); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	_, err := Incus.Bootstrap(context.Background(), in)
	if !errors.Is(err, ErrNode) || !errors.Is(err, nodeprovision.ErrUntrusted) {
		t.Fatalf("second Bootstrap error = %v, want ErrNode and nodeprovision.ErrUntrusted", err)
	}
	if !strings.HasPrefix(err.Error(), "list instances on "+node.Addr()) {
		t.Errorf("second Bootstrap error = %q, want it to name the listing", err)
	}
}
