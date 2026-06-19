terraform {
  source = "../../../modules/incus"
}

inputs = {
  project = "incus"
  env     = "prod"
  node_count = 5
  containers_per_node = 3
}
