terraform {
  source = "../../../modules/incus"
}

inputs = {
  project = "incus"
  env     = "dev"
  node_count = 2
  containers_per_node = 1
}
