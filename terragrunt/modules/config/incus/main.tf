locals {
  schemas = {
    input = {
        nodes = map(object({
            modules = optional(object({
                incus = optional(object({
                    generation = optional(number, 0)
                    clusterIp = string
                    ovnIp = string
                    bootstrap = bool
                }))
            }))
        }))
    }

    output = {
        nodes = map(object({
          modules = optional(object({
              incus = optional(object({
                  generation = number
                  clusterIp = string
                  ovnIp = string
                  bootstrap = bool
                  preseed = string
              }))
          }))
        }))
    }
  }
}

