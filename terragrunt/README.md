This directory contains a Terragrunt "modern" stack-based layout for an
Incus cluster and a management stack (with an OVN load balancer).

Layout
- terragrunt.hcl                # root common config
- stacks/{dev,staging,prod}/    # environment stacks
  - management/                 # management services behind OVN LB
  - incus/                      # incus cluster: vms + containers
- modules/                      # simple placeholder Terraform modules

Notes
- The modules here are intentionally lightweight and use the null provider
  to simulate resources. Replace them with real provider resources (cloud,
  LXD/Incus provider, OVN provider, etc.) when you're ready.

Quick start (example, run from terragrunt/):

  # show plan for dev management stack
  terragrunt run-all plan --terragrunt-working-dir stacks/dev/management

  # apply dev stacks
  terragrunt run-all apply --terragrunt-working-dir stacks/dev

Adjust inputs in each stack's `terragrunt.hcl` or in the module variable
defaults in `modules/`.
