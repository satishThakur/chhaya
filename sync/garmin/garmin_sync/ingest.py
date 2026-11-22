"""Posting to the gateway's localhost ingest listener, with the shared secret."""
import json
import time
import urllib.error
import urllib.request


class IngestError(Exception):
    pass


def post(base_url, secret, path, body, timeout=30, tries=3, sleep=time.sleep):
    """POST JSON, retrying connection errors and 5xx. A 4xx is the gateway saying the payload or
    secret is wrong: retrying cannot help, so it raises at once with the gateway's reason."""
    data = json.dumps(body).encode()
    last = None
    for attempt in range(tries):
        req = urllib.request.Request(base_url.rstrip("/") + path, data=data, method="POST",
                                     headers={"Content-Type": "application/json", "X-Ingest-Secret": secret})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            reason = e.read().decode(errors="replace")[:300]
            if e.code < 500:
                raise IngestError(f"{path}: HTTP {e.code}: {reason}") from None
            last = f"HTTP {e.code}: {reason}"
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            last = f"{type(e).__name__}: {e}"
        if attempt < tries - 1:
            sleep(2 ** attempt)
    raise IngestError(f"{path}: {last}")
