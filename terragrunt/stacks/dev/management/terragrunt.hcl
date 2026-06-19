terraform {
  source = "../../../modules/management"
}

inputs = {
  project = "incus"
  env     = "dev"
  management_count = 1
  services = {
    prometheus = { port = 9090 }
    grafana    = { port = 3000 }
  }
}
