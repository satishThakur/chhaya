# Ubuntu 24.04 LTS, x86_64, resolved from Canonical's public SSM parameter so no AMI
# owner id or filter has to be trusted from memory.
data "aws_ssm_parameter" "ubuntu" {
  name = "/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
}

resource "aws_instance" "box" {
  ami                         = data.aws_ssm_parameter.ubuntu.value
  instance_type               = var.instance_type
  subnet_id                   = sort(data.aws_subnets.default.ids)[0]
  vpc_security_group_ids      = [aws_security_group.box.id]
  iam_instance_profile        = aws_iam_instance_profile.box.name
  associate_public_ip_address = true

  user_data = templatefile("${path.module}/user_data.sh", { repo_url = var.repo_url })

  # IMDSv2 only, and hop limit 1: containers on the box cannot reach the instance
  # role's credentials (and with them the backup bucket). Only the host can.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = var.root_volume_gb
    encrypted   = true
    tags        = { Name = "chhaya-box-root", Snapshot = "chhaya-daily" }
  }

  tags = { Name = "chhaya-box" }

  # All of Chhaya's data lives on this instance's root volume. A new AMI or edited
  # user_data must NOT silently replace the instance and take the data with it.
  # To rebuild on purpose: restore from a snapshot or the backup bucket first.
  lifecycle {
    ignore_changes = [ami, user_data]
  }
}
