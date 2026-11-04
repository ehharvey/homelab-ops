package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiDeployAgentCmd = &cobra.Command{
	Use:   "deploy-agent <instance>",
	Short: "POST /instances/{name}/deploy-agent: put the first app-manager agent on a node",
	Long: `deploy-agent asks the web app to put the first app-manager agent on the
instance's node, over the node's tunnel, with its one-shot bootstrap
credential. It prints the web app's answer: status "deployed" or
"already_deployed", and the agent instance's name.

Unlike the top-level deploy-agent command, which talks to the node directly
with the break-glass cert, this goes through the web app.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.DeployAgent(ctx, args[0])
		})
	},
}

func init() {
	apiCmd.AddCommand(apiDeployAgentCmd)
}
