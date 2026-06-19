locals {
  schemas = {
    input = {
      nodes = map(object({
        modules = optional(object({
          user = optional(object({
            name  = string
            uid   = number
            shell = optional(string, "/bin/bash")
          }))
        }))
      }))
    }

    output = {
      nodes = map(object({
        modules = optional(object({
          user = optional(object({
            name  = string
            uid   = number
            shell = string
          }))
        }))
      }))
    }
  }
}

variable "nodes" {
  type = local.schemas.input.nodes
}

resource "terraform_data" "authorized_keys" {
  for_each = var.nodes
  input = {
    ssh_public_keys = each.value.modules.user.public_keys
  }
}