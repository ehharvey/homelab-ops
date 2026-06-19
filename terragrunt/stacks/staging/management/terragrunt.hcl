terraform {
  source = "../../../modules/management"
}

inputs = {
  project = "incus"
  env     = "staging"
  management_count = 2
  services = {
    prometheus = { port = 9090 }
    grafana    = { port = 3000 }
  }
}
