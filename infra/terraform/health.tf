# The desktop's Garmin sync writes health documents to s3://BUCKET/health/inbox/ and nothing else.
# Garmin refuses sign-ins from AWS, so the sync runs at home; the box pulls the inbox
# (chhayactl health pull) with its own role. docs/health.md, "Where the sync runs".
#
# The key is put-only and prefix-only: a stolen desktop key can add files to the inbox (which the
# gateway's strict decoder then validates) but cannot read your health data, list the bucket, or
# touch backups and traces. The access key itself is NOT created here, because a key made by
# Terraform sits in the state file; create it with the aws CLI (docs/health.md) and keep it in
# deploy/.env.health on the desktop.
resource "aws_iam_user" "health_uploader" {
  name = "chhaya-health-uploader"
}

data "aws_iam_policy_document" "health_uploader" {
  statement {
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.backup.arn}/health/inbox/*"]
  }
}

resource "aws_iam_user_policy" "health_uploader" {
  name   = "put-health-inbox"
  user   = aws_iam_user.health_uploader.name
  policy = data.aws_iam_policy_document.health_uploader.json
}
