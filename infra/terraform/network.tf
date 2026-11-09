# Default VPC to start (docs/deployment.md). The instance gets a public IP so it can
# reach Telegram, the model API and the web without a NAT gateway; the security group
# has no inbound rules, so nothing can reach it.

data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
  filter {
    name   = "default-for-az"
    values = ["true"]
  }
}

resource "aws_security_group" "box" {
  name_prefix = "chhaya-box-"
  description = "Chhaya box: no inbound. Outbound web, DNS."
  vpc_id      = data.aws_vpc.default.id

  # Deliberately no ingress blocks. Admin access is SSM Session Manager, which is
  # outbound from the instance.

  egress {
    description = "HTTPS: Telegram, model API, GitHub, S3, SSM, Docker Hub"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    description = "HTTP: Ubuntu apt mirrors are http (packages are signature-checked)"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    description = "DNS"
    from_port   = 53
    to_port     = 53
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    description = "DNS over TCP"
    from_port   = 53
    to_port     = 53
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  lifecycle {
    create_before_destroy = true
  }
}
