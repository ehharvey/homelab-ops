terraform {
  source = "./modules//?ref=local"
}

locals {
  # common tags and settings
  project = "incus-cluster"
  region  = "local"
}

inputs = {
  project = local.project
  region  = local.region
}
