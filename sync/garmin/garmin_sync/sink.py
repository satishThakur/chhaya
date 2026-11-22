"""The S3 sink: instead of posting to the gateway, write each document to a bucket prefix the box
polls (chhayactl health pull). The documents are exactly what /ingest/day and /ingest/run accept, so
the box validates them with the same strict decoder; this side only moves bytes."""
import hashlib
import json
import re

from . import ingest, s3put


def split_uri(uri):
    m = re.fullmatch(r"s3://([a-z0-9][a-z0-9.\-]{2,62})/(.+?)/?", uri or "")
    if not m:
        raise SystemExit("HEALTH_S3_URI must look like s3://bucket/health/inbox")
    return m.group(1), m.group(2)


def key_for(prefix, path, body):
    """day-<date>-<hash>.json or run-<finished>-<hash>.json. The hash makes the key the content: the
    same day posted twice with no change is the same object, a changed day is a new one. 'day-' sorts
    before 'run-', so the box ingests days first and the run record after them."""
    raw = json.dumps(body, sort_keys=True, separators=(",", ":")).encode()
    h = hashlib.sha256(raw).hexdigest()[:10]
    if path == "/ingest/day":
        name = f"day-{body['date']}-{h}.json"
    elif path == "/ingest/run":
        name = "run-" + re.sub(r"[^0-9A-Za-z]", "", body["finished_at"]) + f"-{h}.json"
    else:
        raise ValueError(f"unknown ingest path {path}")
    return f"{prefix}/{name}", raw


def make(uri, client=None):
    """-> a post(url, secret, path, body) with the same shape as ingest.post, writing to S3. url and
    secret are ignored: the destination is the bucket."""
    bucket, prefix = split_uri(uri)
    if client is None:
        client = s3put.Client()

    def post(_url, _secret, path, body, **_):
        key, raw = key_for(prefix, path, body)
        try:
            client.put_object(Bucket=bucket, Key=key, Body=raw, ContentType="application/json",
                              ServerSideEncryption="AES256")
        except Exception as e:  # botocore errors; named, never the credentials
            raise ingest.IngestError(f"{path}: S3 put failed: {type(e).__name__}: {str(e)[:200]}") from None
        return {"key": key}

    return post
