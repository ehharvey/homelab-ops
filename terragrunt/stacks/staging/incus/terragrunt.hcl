terraform {
  source = "../../../modules/incus"
}

inputs = {
  project = "incus"
  env     = "staging"
  node_count = 3
  containers_per_node = 2
}
