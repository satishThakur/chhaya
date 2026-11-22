import unittest
from datetime import datetime, timezone

from garmin_sync import ingest, main, transform

NOW = datetime(2026, 11, 22, 6, 0, tzinfo=timezone.utc)  # 11:30 IST on the 22nd
CFG = {"email": "me@example.com", "password": "pw-never-logged", "secret": "s" * 20, "url": "http://gw:8444",
       "tz": "Asia/Kolkata", "tokendir": "/tokens", "every": 180}

GOOD = {"sleep": {"dailySleepDTO": {"sleepTimeSeconds": 25000}}, "hrv": None, "stress": {"avgStressLevel": 20},
        "daily_stats": {"totalSteps": 5000}, "body_battery": [], "activities": []}


class Poster:
    def __init__(self, fail_path=None):
        self.calls, self.fail_path = [], fail_path

    def __call__(self, url, secret, path, body, **kw):
        self.calls.append((path, body))
        if path == self.fail_path:
            raise ingest.IngestError(f"{path}: HTTP 400: nope")
        return {}


class RunOnce(unittest.TestCase):
    def run_it(self, raw=None, post=None, login=None, **kw):
        post = post or Poster()
        return main.run_once(CFG, now=NOW, garmin_login=login or (lambda *a: object()),
                             fetch=lambda c, d: dict(raw or GOOD), post=post, **kw), post

    def test_a_good_pass_posts_both_days_and_reports_ok(self):
        run, post = self.run_it()
        self.assertEqual(run["status"], "ok")
        self.assertEqual(run["days"], ["2026-11-21", "2026-11-22"])
        paths = [p for p, _ in post.calls]
        self.assertEqual(paths, ["/ingest/day", "/ingest/day", "/ingest/run"])
        self.assertEqual(post.calls[0][1]["date"], "2026-11-21")
        self.assertEqual(run["metrics"], 6)  # (1 sleep + 1 stress + 1 steps) per day

    def test_a_backfill_fetches_the_older_days_first_and_each_day_once(self):
        run, post = self.run_it(back=3)
        self.assertEqual(run["days"], ["2026-11-19", "2026-11-20", "2026-11-21", "2026-11-22"])
        self.assertEqual([b["date"] for p, b in post.calls if p == "/ingest/day"], run["days"])

    def test_a_failed_login_is_reported_not_swallowed_and_never_logs_the_password(self):
        def bad(*a):
            raise RuntimeError("401 for me@example.com")
        run, post = self.run_it(login=bad)
        self.assertEqual(run["status"], "failed")
        self.assertTrue(run["errors"][0].startswith("login:"))
        self.assertEqual([p for p, _ in post.calls], ["/ingest/run"])
        self.assertNotIn("pw-never-logged", str(post.calls))

    def test_a_section_that_raised_is_an_error_and_the_rest_is_still_posted(self):
        raw = dict(GOOD, stress=ConnectionError("timeout"))
        run, post = self.run_it(raw=raw)
        self.assertEqual(run["status"], "partial")
        self.assertTrue(any("stress: ConnectionError" in e for e in run["errors"]))
        day = [b for p, b in post.calls if p == "/ingest/day"][0]
        self.assertIn("sleep", day)
        self.assertNotIn("stress", day)

    def test_no_data_at_all_is_ok_and_posts_no_days(self):
        run, post = self.run_it(raw={k: None for k in GOOD})
        self.assertEqual(run["status"], "ok")
        self.assertEqual([p for p, _ in post.calls], ["/ingest/run"])

    def test_every_day_refused_by_the_gateway_is_failed(self):
        run, _ = self.run_it(post=Poster(fail_path="/ingest/day"))
        self.assertEqual(run["status"], "failed")
        self.assertEqual(len([e for e in run["errors"] if "post:" in e]), 2)

    def test_a_dry_run_posts_nothing_at_all(self):
        run, post = self.run_it(dry_run=True)
        self.assertEqual(post.calls, [])
        self.assertEqual(run["status"], "ok")
        self.assertEqual(run["metrics"], 6)

    def test_an_unexpected_response_shape_is_an_error_not_a_crash(self):
        raw = dict(GOOD, sleep="not a dict")
        run, _ = self.run_it(raw=raw)
        self.assertTrue(any(e.startswith("2026-11-21 sleep: unreadable") for e in run["errors"]))


class Config(unittest.TestCase):
    def test_missing_secrets_stop_the_start(self):
        with self.assertRaises(SystemExit):
            main.config_from_env({"GARMIN_EMAIL": "a"})

    def test_defaults(self):
        c = main.config_from_env({"GARMIN_EMAIL": "a", "GARMIN_PASSWORD": "b", "CHHAYA_HEALTH_INGEST_SECRET": "c" * 20})
        self.assertEqual((c["url"], c["tz"], c["every"]), ("http://gateway:8444", "Asia/Kolkata", 180))


class Post(unittest.TestCase):
    def test_a_4xx_is_not_retried(self):
        import urllib.error, io
        calls = []

        def fake_open(req, timeout=0):
            calls.append(1)
            raise urllib.error.HTTPError(req.full_url, 400, "bad", {}, io.BytesIO(b'{"error":"stress_avg out of range"}'))
        orig = ingest.urllib.request.urlopen
        ingest.urllib.request.urlopen = fake_open
        try:
            with self.assertRaises(ingest.IngestError) as cm:
                ingest.post("http://x", "s", "/ingest/day", {}, sleep=lambda s: None)
        finally:
            ingest.urllib.request.urlopen = orig
        self.assertEqual(len(calls), 1)
        self.assertIn("stress_avg", str(cm.exception))

    def test_a_5xx_is_retried_then_raised(self):
        import urllib.error, io
        calls = []

        def fake_open(req, timeout=0):
            calls.append(1)
            raise urllib.error.HTTPError(req.full_url, 502, "bad", {}, io.BytesIO(b"{}"))
        orig = ingest.urllib.request.urlopen
        ingest.urllib.request.urlopen = fake_open
        try:
            with self.assertRaises(ingest.IngestError):
                ingest.post("http://x", "s", "/ingest/day", {}, tries=3, sleep=lambda s: None)
        finally:
            ingest.urllib.request.urlopen = orig
        self.assertEqual(len(calls), 3)


if __name__ == "__main__":
    unittest.main()
