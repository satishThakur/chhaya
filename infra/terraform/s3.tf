# Backups and the one-time data handover. Private, encrypted, versioned.
resource "aws_s3_bucket" "backup" {
  bucket = "chhaya-backup-${data.aws_caller_identity.me.account_id}-${var.region}"
}

resource "aws_s3_bucket_public_access_block" "backup" {
  bucket                  = aws_s3_bucket.backup.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "backup" {
  bucket = aws_s3_bucket.backup.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "backup" {
  bucket = aws_s3_bucket.backup.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_versioning" "backup" {
  bucket = aws_s3_bucket.backup.id
  versioning_configuration {
    status = "Enabled"
  }
}

# Versioning keeps deleted and overwritten objects. These rules stop them piling up, prefix
# by prefix.
#
# DO NOT turn these back into one bucket-wide rule (filter {}). An S3 lifecycle filter can
# include a prefix but cannot exclude one, so a bucket-wide noncurrent-version expiry would
# also delete the old versions of everything under evals/ (annotations, eval cases and
# results) after 30 days. evals/ is deliberately in no expiry rule: it is the owner's labelled
# data, it is tiny, and an overwritten or deleted annotation must stay recoverable.
# docs/decisions.md, 2026-11-10.
locals {
  # Prefixes whose old versions are not worth keeping forever. A new top-level prefix gets no
  # expiry until it is listed here, which fails safe: nothing is deleted by omission.
  expiring_prefixes = ["handover/", "terraform/", "tools/"]
}

resource "aws_s3_bucket_lifecycle_configuration" "backup" {
  bucket     = aws_s3_bucket.backup.id
  depends_on = [aws_s3_bucket_versioning.backup]

  # Applies to every prefix, including evals/: it only cleans up failed uploads.
  rule {
    id     = "abort-incomplete-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  # Nightly backups (chhayactl backup push) are kept 30 days. Versioning then keeps the
  # expired object as a noncurrent version for 30 more days before it is gone for good.
  rule {
    id     = "expire-nightly-backups"
    status = "Enabled"
    filter {
      prefix = "backups/"
    }
    expiration {
      days = 30
    }
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
  }

  dynamic "rule" {
    for_each = toset(local.expiring_prefixes)
    content {
      id     = "expire-old-versions-${trimsuffix(rule.value, "/")}"
      status = "Enabled"
      filter {
        prefix = rule.value
      }
      noncurrent_version_expiration {
        noncurrent_days = 30
      }
    }
  }
}
