terraform {
  source = "../../../modules/management"
}

inputs = {
  project = "incus"
  env     = "prod"
  management_count = 3
  services = {
    prometheus = { port = 9090 }
    grafana    = { port = 3000 }
    alertmanager = { port = 9093 }
  }
}
