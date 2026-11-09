output "instance_id" {
  value = aws_instance.box.id
}

output "backup_bucket" {
  value = aws_s3_bucket.backup.id
}

output "ssm_session" {
  description = "Open a shell on the box."
  value       = "aws ssm start-session --region ${var.region} --target ${aws_instance.box.id}"
}
