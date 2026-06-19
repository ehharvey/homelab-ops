
terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
}

variable "project" {
  type = string
}

variable "env" {
  type = string
}

variable "management_count" {
  type    = number
  default = 2
}

variable "services" {
  type = map(any)
  default = {
    prometheus = { port = 9090 }
  }
}

resource "null_resource" "ovn_load_balancer" {
  triggers = {
    project = var.project
    env     = var.env
  }
}

# create a small set of management nodes (placeholder)
resource "null_resource" "management_node" {
  count = var.management_count
  triggers = {
    env      = var.env
    project  = var.project
    services = jsonencode(var.services)
    idx      = tostring(count.index)
  }
}

output "ovn_lb_id" {
  value = "ovn-lb-${var.project}-${var.env}"
}

output "management_nodes" {
  value = [for n in null_resource.management_node : n.triggers]
}

output "services" {
  value = var.services
}
