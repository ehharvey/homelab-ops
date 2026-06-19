locals {
  schemas = {
    input = {
        nodes = map(object({
            modules = {
                ovn = object({
                    generation = optional(number, 0)
                    ipAddress = string
                })
            }
        }))
    }
    
    output = {
        nodes = map(object({
          modules = {
              ovn = object({
                  generation = number
                  ipAddress = string
              })
          }
        }))
    }
  }

  derived = {
    ovn-northd-nb-db = join(",", [for node in var.nodes : "${node.modules.ovn.ipAddress}:6641"])
    ovn-northd-sb-db = join(",", [for node in var.nodes : "${node.modules.ovn.ipAddress}:6642"])
  }
}

variable "nodes" {
  type = local.schemas.input.nodes
}

resource "terraform_data" "etc-defaults-ovn" {
  for_each = var.nodes
  input = templatefile("${path.module}/files/etc/default/ovn-central", {
    modules = each.value.modules
    derived = local.derived
  })
}