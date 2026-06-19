locals {
  schemas = {
    input = {
      nodes = map(object({
        modules = optional(object({
          ssh = optional(object({
            generation = optional(number, 0)
            listens = set(object({
              port    = optional(number, 22)
              address = string
            }))
          }))
        }))
      }))
    }

    output = {
      nodes = map(object({
        modules = optional(object({
          ssh = optional(object({
            generation = number
            listens = set(object({
              port    = number
              address = string
            }))
            sshd_config = string
          }))
        }))
      }))
    }
  }
}

variable "nodes" {
  type = local.schemas.input.nodes
}

