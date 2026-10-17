package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
)

var (
	deployAgentFiles    []string
	deployAgentInstance string
	deployAgentCertPath string
	deployAgentKeyPath  string
	deployAgentAddr     string
	deployAgentRepoURL  string
	deployAgentRepoRef  string
	deployAgentTimeout  time.Duration
)

var deployAgentCmd = &cobra.Command{
	Use:   "deploy-agent",
	Short: "Deploy the app-manager agent onto a node directly over the LAN, with the break-glass cert",
	Long: `deploy-agent creates the app-manager agent's first instance
(<app>-<instance>-g0) on a node, talking to the node's Incus API directly at
--addr with the break-glass cert from gen-cert. It needs no web app: use it
for initial bring-up before the node's WireGuard tunnel is enrolled, or to
recover when every agent is lost (docs/Decisions.md §27). The web app's
POST /instances/{name}/deploy-agent is the other path, over the tunnel.

--file is the fleet definition: the target Instance and the agent App
(kind: App, type: agent, replicas: per-node). Repeat it to merge several
files, e.g. examples/fleet/instances.yaml and apps.yaml.

If any of the agent's instances already exists on the node, nothing is
created. The break-glass cert is never revoked.`,
	RunE: runDeployAgent,
}

func init() {
	deployAgentCmd.Flags().StringArrayVar(&deployAgentFiles, "file", nil, "path to a fleet definition YAML file (required; repeat to merge several)")
	deployAgentCmd.Flags().StringVar(&deployAgentInstance, "instance", "", "name of the Instance to deploy to (required if --file declares more than one)")
	deployAgentCmd.Flags().StringVar(&deployAgentCertPath, "cert", "./bootstrap-output/cert/client.crt", "path to the break-glass client certificate (gen-cert's output)")
	deployAgentCmd.Flags().StringVar(&deployAgentKeyPath, "key", "./bootstrap-output/cert/client.key", "path to the break-glass client key (gen-cert's output)")
	deployAgentCmd.Flags().StringVar(&deployAgentAddr, "addr", "", "the node's Incus API host:port on the LAN, e.g. 192.168.1.201:8443 (required)")
	deployAgentCmd.Flags().StringVar(&deployAgentRepoURL, "config-repo-url", "", "the config repo the agent syncs from, its CONFIG_REPO_URL (required)")
	deployAgentCmd.Flags().StringVar(&deployAgentRepoRef, "config-repo-ref", "main", "the config repo ref the agent syncs, its CONFIG_REPO_REF")
	deployAgentCmd.Flags().DurationVar(&deployAgentTimeout, "timeout", 10*time.Minute, "how long to wait for the node to pull the image and create the instance")

	rootCmd.AddCommand(deployAgentCmd)
}

func runDeployAgent(cmd *cobra.Command, _ []string) error {
	if len(deployAgentFiles) == 0 {
		return fmt.Errorf("--file is required")
	}
	if deployAgentAddr == "" {
		return fmt.Errorf("--addr is required")
	}
	if deployAgentRepoURL == "" {
		return fmt.Errorf("--config-repo-url is required")
	}

	cfg, err := loadFleetFiles(deployAgentFiles)
	if err != nil {
		return err
	}
	if issues := config.Validate(cfg); !issues.Empty() {
		return fmt.Errorf("invalid config: %w", issues)
	}
	inst, err := selectInstance(cfg.Instances, deployAgentInstance)
	if err != nil {
		return err
	}
	app, ok := agentdeploy.FindApp(cfg.Apps)
	if !ok {
		return fmt.Errorf("no agent App declared (kind: App, type: %s, replicas: per-node)", agentdeploy.AppType)
	}

	certPEM, err := os.ReadFile(deployAgentCertPath) //nolint:gosec // path is operator-supplied via --cert, not untrusted external input
	if err != nil {
		return fmt.Errorf("read client cert (run gen-cert first, or pass --cert): %w", err)
	}
	keyPEM, err := os.ReadFile(deployAgentKeyPath) //nolint:gosec // path is operator-supplied via --key, not untrusted external input
	if err != nil {
		return fmt.Errorf("read client key (run gen-cert first, or pass --key): %w", err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), deployAgentTimeout)
	defer cancel()
	// Direct over the LAN, not through a WireGuard tunnel: this path exists
	// for nodes the web app can't reach.
	dial := (&net.Dialer{}).DialContext

	existing, err := nodeprovision.InstanceNames(ctx, dial, deployAgentAddr, certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("list instances on %s: %w", deployAgentAddr, err)
	}
	for _, name := range existing {
		if agentdeploy.IsAgentInstance(app, inst, name) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "already deployed: %s on %s\n", name, inst.Name)
			return nil
		}
	}

	req := agentdeploy.Desired(app, inst, agentdeploy.Repo{URL: deployAgentRepoURL, Ref: deployAgentRepoRef})
	if err := nodeprovision.CreateInstanceNoRevoke(ctx, dial, deployAgentAddr, certPEM, keyPEM, req); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deployed %s on %s\n", req.Name, inst.Name)
	return nil
}

// loadFleetFiles parses each path and merges the results.
func loadFleetFiles(paths []string) (config.Config, error) {
	var cfg config.Config
	for _, path := range paths {
		f, err := os.Open(path) //nolint:gosec // path is operator-supplied via --file, not untrusted external input
		if err != nil {
			return config.Config{}, fmt.Errorf("open config file: %w", err)
		}
		part, err := config.Parse(f)
		_ = f.Close()
		if err != nil {
			return config.Config{}, fmt.Errorf("parse config file %s: %w", path, err)
		}
		cfg.Append(part)
	}
	return cfg, nil
}

// selectInstance picks the Instance named name, or the only one if name is
// empty.
func selectInstance(instances []config.Instance, name string) (config.Instance, error) {
	if name == "" {
		if len(instances) != 1 {
			return config.Instance{}, fmt.Errorf("expected exactly 1 Instance, got %d (pass --instance to choose one)", len(instances))
		}
		return instances[0], nil
	}
	for _, inst := range instances {
		if inst.Name == name {
			return inst, nil
		}
	}
	return config.Instance{}, fmt.Errorf("no Instance named %q", name)
}
