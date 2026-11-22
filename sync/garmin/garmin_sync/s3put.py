"""A PUT of one object to S3, signed with AWS Signature Version 4, using only the standard library.
The sync needs exactly this one call; boto3 would be a 70 MB dependency for it (and its download
failed twice on the desktop's connection). Credentials come from AWS_ACCESS_KEY_ID and
AWS_SECRET_ACCESS_KEY, which in the container are the put-only uploader's."""
import hashlib
import hmac
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone


class S3Error(Exception):
    pass


def _hmac(key, msg):
    return hmac.new(key, msg.encode(), hashlib.sha256).digest()


def sign(method, host, path, headers, payload_hash, access_key, secret_key, region, amz_date, service="s3"):
    """-> the Authorization header value. `headers` are the ones to sign (lowercase names), and must
    include host, x-amz-date and x-amz-content-sha256."""
    day = amz_date[:8]
    names = sorted(headers)
    canonical_headers = "".join(f"{n}:{' '.join(str(headers[n]).split())}\n" for n in names)
    signed = ";".join(names)
    canonical = "\n".join([method, urllib.parse.quote(path, safe="/-_.~"), "", canonical_headers, signed, payload_hash])
    scope = f"{day}/{region}/{service}/aws4_request"
    to_sign = "\n".join(["AWS4-HMAC-SHA256", amz_date, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    k = _hmac(_hmac(_hmac(_hmac(("AWS4" + secret_key).encode(), day), region), service), "aws4_request")
    sig = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()
    return f"AWS4-HMAC-SHA256 Credential={access_key}/{scope}, SignedHeaders={signed}, Signature={sig}"


class Client:
    """Has the one method the sink uses: put_object(Bucket, Key, Body, ContentType, ServerSideEncryption)."""

    def __init__(self, env=os.environ, opener=urllib.request.urlopen, sleep=time.sleep, now=None):
        self.access = env.get("AWS_ACCESS_KEY_ID", "")
        self.secret = env.get("AWS_SECRET_ACCESS_KEY", "")
        self.region = env.get("AWS_DEFAULT_REGION") or "ap-south-1"
        if not (self.access and self.secret):
            raise SystemExit("S3 mode needs HEALTH_AWS_ACCESS_KEY_ID and HEALTH_AWS_SECRET_ACCESS_KEY")
        self.opener, self.sleep = opener, sleep
        self.now = now or (lambda: datetime.now(timezone.utc))

    def put_object(self, Bucket, Key, Body, ContentType="application/octet-stream", ServerSideEncryption=None, tries=3):
        host = f"{Bucket}.s3.{self.region}.amazonaws.com"
        path = "/" + Key
        last = None
        for attempt in range(tries):
            amz_date = self.now().strftime("%Y%m%dT%H%M%SZ")
            phash = hashlib.sha256(Body).hexdigest()
            h = {"host": host, "x-amz-content-sha256": phash, "x-amz-date": amz_date, "content-type": ContentType}
            if ServerSideEncryption:
                h["x-amz-server-side-encryption"] = ServerSideEncryption
            auth = sign("PUT", host, path, h, phash, self.access, self.secret, self.region, amz_date)
            send = {k: v for k, v in h.items() if k != "host"}
            send["Authorization"] = auth
            req = urllib.request.Request(f"https://{host}{urllib.parse.quote(path, safe='/-_.~')}", data=Body, method="PUT", headers=send)
            try:
                with self.opener(req, timeout=30):
                    return
            except urllib.error.HTTPError as e:
                code = _error_code(e)
                if e.code < 500:
                    raise S3Error(f"HTTP {e.code} {code}") from None
                last = f"HTTP {e.code} {code}"
            except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
                last = f"{type(e).__name__}"
            if attempt < tries - 1:
                self.sleep(2 ** attempt)
        raise S3Error(last)


def _error_code(e):
    """The <Code> of S3's XML error body (AccessDenied, SignatureDoesNotMatch, ...), never more."""
    try:
        body = e.read(2000).decode(errors="replace")
        a, b = body.index("<Code>") + 6, body.index("</Code>")
        return body[a:b][:60]
    except Exception:
        return ""
