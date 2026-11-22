import hashlib
import unittest
import urllib.error
from datetime import datetime, timezone

from garmin_sync import s3put


class Sign(unittest.TestCase):
    def test_aws_documented_put_object_example(self):
        # "Example: PUT Object" in AWS's Signature Version 4 documentation (header-based auth).
        body = b"Welcome to Amazon S3."
        ph = hashlib.sha256(body).hexdigest()
        self.assertEqual(ph, "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072")
        headers = {"date": "Fri, 24 May 2013 00:00:00 GMT", "host": "examplebucket.s3.amazonaws.com",
                   "x-amz-content-sha256": ph, "x-amz-date": "20130524T000000Z", "x-amz-storage-class": "REDUCED_REDUNDANCY"}
        auth = s3put.sign("PUT", "examplebucket.s3.amazonaws.com", "/test$file.text", headers, ph,
                          "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "20130524T000000Z")
        self.assertTrue(auth.endswith("Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"), auth)
        self.assertIn("SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class", auth)


class Resp:
    def __enter__(self): return self
    def __exit__(self, *a): return False


class Put(unittest.TestCase):
    ENV = {"AWS_ACCESS_KEY_ID": "AKIATEST", "AWS_SECRET_ACCESS_KEY": "s" * 40, "AWS_DEFAULT_REGION": "ap-south-1"}
    NOW = lambda self: datetime(2026, 11, 22, 6, 0, 0, tzinfo=timezone.utc)

    def client(self, opener):
        return s3put.Client(self.ENV, opener=opener, sleep=lambda s: None, now=self.NOW)

    def test_a_put_is_one_signed_request_to_the_bucket_host_and_never_carries_the_secret(self):
        seen = []
        def opener(req, timeout):
            seen.append(req)
            return Resp()
        self.client(opener).put_object(Bucket="my-bucket", Key="health/inbox/day-2026-11-22-ab.json", Body=b"{}",
                                       ContentType="application/json", ServerSideEncryption="AES256")
        r = seen[0]
        self.assertEqual(r.full_url, "https://my-bucket.s3.ap-south-1.amazonaws.com/health/inbox/day-2026-11-22-ab.json")
        self.assertEqual(r.get_method(), "PUT")
        self.assertEqual(r.get_header("X-amz-server-side-encryption"), "AES256")
        self.assertIn("Credential=AKIATEST/20261122/ap-south-1/s3/aws4_request", r.get_header("Authorization"))
        self.assertNotIn("s" * 40, str(r.header_items()))

    def test_a_4xx_is_raised_at_once_with_the_s3_code_and_a_5xx_is_retried(self):
        calls = []
        def denied(req, timeout):
            calls.append(1)
            raise urllib.error.HTTPError(req.full_url, 403, "x", {}, __import__("io").BytesIO(b"<Error><Code>AccessDenied</Code></Error>"))
        with self.assertRaises(s3put.S3Error) as c:
            self.client(denied).put_object(Bucket="b", Key="k", Body=b"x")
        self.assertIn("AccessDenied", str(c.exception))
        self.assertEqual(len(calls), 1)
        calls.clear()
        def down(req, timeout):
            calls.append(1)
            raise urllib.error.HTTPError(req.full_url, 503, "x", {}, __import__("io").BytesIO(b""))
        with self.assertRaises(s3put.S3Error):
            self.client(down).put_object(Bucket="b", Key="k", Body=b"x")
        self.assertEqual(len(calls), 3)

    def test_no_credentials_stops_the_start(self):
        with self.assertRaises(SystemExit):
            s3put.Client({})
