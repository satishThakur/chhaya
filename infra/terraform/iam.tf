data "aws_caller_identity" "me" {}

data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "box" {
  name_prefix        = "chhaya-box-"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.box.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "backup_bucket" {
  statement {
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.backup.arn]
  }
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${aws_s3_bucket.backup.arn}/*"]
  }
}

resource "aws_iam_role_policy" "backup_bucket" {
  name   = "backup-bucket"
  role   = aws_iam_role.box.id
  policy = data.aws_iam_policy_document.backup_bucket.json
}

resource "aws_iam_instance_profile" "box" {
  name_prefix = "chhaya-box-"
  role        = aws_iam_role.box.name
}
