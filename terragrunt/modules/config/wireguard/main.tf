locals {
  schemas = {
    input = {
        nodes = map(object({
            modules = optional(object({
                wireguard = optional(object({
                    generation = optional(number, 0)
                }))
            }))
        }))
    }

    output = {
        nodes = map(object({
          modules = optional(object({
              wireguard = optional(object({
                  private_key = string
                  public_key = string
                  generation = number
              }))
          }))
        }))
    }
  }
}

variable "nodes" {
  type = local.schemas.input.nodes
}

data "external" "wireguard_private_key_generator" {
  for_each = var.nodes
  program = ["wg", "genkey"]
}

data "external" "wireguard_public_key_generator" {
  for_each = var.nodes
  program = ["wg", "pubkey"]
  query = {
    private_key = data.external.wireguard_private_key_generator[each.key].result
  }
}

resource "terraform_data" "wireguard_private_keys" {
  for_each = var.nodes
  input = {
    private_key = data.external.wireguard_private_key_generator[each.key].result
  }
  
  ignore_changes = [input]
  triggers_replace = each.value.modules.wireguard.generation
}

resource "terraform_data" "wireguard_public_keys" {
  for_each = var.nodes
  input = {
    private_key = terraform_data.wireguard_private_key[each.key].result.private_key
  }
  
  ignore_changes = [input]
  triggers_replace = each.value.modules.wireguard.generation
}

output "nodes" {
  value = {
    for k, v in var.nodes : k => {
      modules = merge(
        v.modules,
        {
          wireguard = {
            private_key = terraform_data.wireguard_private_key[k].result.private_key
            public_key = terraform_data.wireguard_public_key[k].result.public_key
            generation = v.modules.wireguard.generation
          }
        }
      )
    }
  }
}