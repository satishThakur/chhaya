variable "region" {
  description = "AWS region. Mumbai, per docs/deployment.md."
  type        = string
  default     = "ap-south-1"
}

variable "instance_type" {
  description = "x86_64 only: the pinned Hermes image is amd64-only (single-platform digest). Resize after measuring."
  type        = string
  default     = "t3.medium"
}

variable "root_volume_gb" {
  type    = number
  default = 30
}

variable "repo_url" {
  description = "Public repo, cloned over HTTPS on first boot."
  type        = string
  default     = "https://github.com/satishthakur/chhaya.git"
}

variable "snapshot_retain_days" {
  type    = number
  default = 14
}
