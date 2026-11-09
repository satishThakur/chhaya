terraform {
  required_version = ">= 1.10" # use_lockfile (S3-native locking) needs 1.10

  # State lives in the backup bucket this configuration creates (a chicken and egg
  # solved once, by hand: created with local state, then migrated with
  # `terraform init -migrate-state`). Bucket is private, encrypted and versioned.
  # Backend blocks cannot use variables, so the name is written out.
  backend "s3" {
    bucket       = "chhaya-backup-803874683069-ap-south-1"
    key          = "terraform/chhaya-box.tfstate"
    region       = "ap-south-1"
    encrypt      = true
    use_lockfile = true
  }
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { Project = "chhaya", ManagedBy = "terraform" }
  }
}
