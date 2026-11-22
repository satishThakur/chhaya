"""The sync loop: sign in, fetch yesterday and today, post each day, report the run."""
import argparse
import logging
import os
import sys
import time
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo

from . import client as garmin
from . import ingest, sink, transform

log = logging.getLogger("garmin_sync")

SECTIONS = (
    ("sleep", transform.sleep),
    ("hrv", transform.hrv),
    ("stress", transform.stress),
    ("daily_stats", transform.daily_stats),
    ("body_battery", transform.body_battery),
    ("activities", transform.activities),
)


def _short(e):
    return f"{type(e).__name__}: {str(e)[:120]}"


def build_day(date, raw):
    """-> (document, errors). A section that raised is an error; one that returned nothing is just
    no data."""
    parts, errors = {}, []
    for name, fn in SECTIONS:
        r = raw.get(name)
        if isinstance(r, Exception):
            errors.append(f"{name}: {_short(r)}")
            continue
        try:
            parts[name] = fn(r)
        except Exception as e:  # a response shape we did not expect
            errors.append(f"{name}: unreadable response ({_short(e)})")
    return transform.day_document(date, parts), errors


def count(doc):
    # Numbers only: a section's timestamps (sleep's ended_at) are not metrics.
    metrics = sum(1 for v in doc.values() if isinstance(v, dict) for x in v.values() if isinstance(x, (int, float)))
    return metrics, len(doc.get("activities", []))


def run_once(cfg, now=None, garmin_login=garmin.login, fetch=garmin.fetch, post=ingest.post, dry_run=False, back=1, sleep=time.sleep):
    if cfg.get("s3_uri") and post is ingest.post and not dry_run:
        post = sink.make(cfg["s3_uri"])
    tz = ZoneInfo(cfg["tz"])
    now = now or datetime.now(timezone.utc)
    today = now.astimezone(tz).date()
    # Oldest first: today and `back` days before it. Re-posting a day replaces it, so any window is safe.
    days = [(today - timedelta(days=n)).isoformat() for n in range(back, -1, -1)]
    started = now
    errors, metrics, acts, posted = [], 0, 0, 0

    def finish(status):
        run = {"started_at": started.strftime("%Y-%m-%dT%H:%M:%SZ"),
               "finished_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
               "status": status, "days": days, "metrics": metrics, "activities": acts, "errors": errors[:20]}
        if dry_run:
            log.info("dry run: would report %s", {k: run[k] for k in ("status", "days", "metrics", "activities", "errors")})
            return run
        try:
            post(cfg["url"], cfg["secret"], "/ingest/run", run)
        except ingest.IngestError as e:
            log.error("could not report the run: %s", e)
        return run

    try:
        client = garmin_login(cfg["email"], cfg["password"], cfg["tokendir"])
    except Exception as e:
        errors.append(f"login: {_short(e)}")
        return finish("failed")

    for i, date in enumerate(days):
        if i and back > 2:
            sleep(2)  # a long backfill is hundreds of calls; Garmin answers a burst with 429
        doc, errs = build_day(date, fetch(client, date))
        errors += [f"{date} {e}" for e in errs]
        if transform.is_empty(doc):
            continue
        m, a = count(doc)
        if dry_run:
            log.info("dry run: %s would post %d metrics, %d activities (sections: %s)", date, m, a, ", ".join(k for k in doc if k != "date"))
            metrics, acts, posted = metrics + m, acts + a, posted + 1
            continue
        try:
            post(cfg["url"], cfg["secret"], "/ingest/day", doc)
            metrics, acts, posted = metrics + m, acts + a, posted + 1
        except ingest.IngestError as e:
            errors.append(f"{date} post: {e}")
    if errors:
        return finish("partial" if posted else "failed")
    return finish("ok")


def config_from_env(env=os.environ):
    # With HEALTH_S3_URI the documents go to a bucket and no gateway secret is needed; without it
    # they are posted to the gateway's ingest listener.
    need = ["GARMIN_EMAIL", "GARMIN_PASSWORD"] + ([] if env.get("HEALTH_S3_URI") else ["CHHAYA_HEALTH_INGEST_SECRET"])
    missing = [k for k in need if not env.get(k)]
    if missing:
        raise SystemExit("missing environment: " + ", ".join(missing))
    return {"email": env["GARMIN_EMAIL"], "password": env["GARMIN_PASSWORD"], "secret": env.get("CHHAYA_HEALTH_INGEST_SECRET", ""), "s3_uri": env.get("HEALTH_S3_URI", ""),
            "url": env.get("CHHAYA_HEALTH_INGEST_URL", "http://gateway:8444"), "tz": env.get("CHHAYA_HEALTH_TZ", "Asia/Kolkata"),
            "tokendir": env.get("GARMIN_TOKENDIR", "/tokens"), "every": int(env.get("SYNC_EVERY_MINUTES", "180"))}


def main(argv=None):
    ap = argparse.ArgumentParser(description="Garmin -> gateway health sync")
    ap.add_argument("--once", action="store_true", help="one pass, then exit (non-zero unless it fully succeeded)")
    ap.add_argument("--dry-run", action="store_true", help="fetch and show what would be posted; post nothing (implies --once)")
    ap.add_argument("--days", type=int, default=None, metavar="N", help="also fetch the N days before today (the loop uses 2; use e.g. 30 once to backfill). Implies --once")
    args = ap.parse_args(argv)
    if args.days is not None and not 1 <= args.days <= 365:
        ap.error("--days must be between 1 and 365")
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    cfg = config_from_env()
    if args.once or args.dry_run or args.days is not None:
        run = run_once(cfg, dry_run=args.dry_run, back=args.days or 2)
        return 0 if run["status"] == "ok" else 1
    while True:
        try:
            run = run_once(cfg, back=2)  # the box may have been off a day; re-posting a day is safe
            log.info("pass finished: %s (%d metrics, %d activities, %d errors)", run["status"], run["metrics"], run["activities"], len(run["errors"]))
        except Exception:
            log.exception("a pass crashed")
        time.sleep(cfg["every"] * 60)


if __name__ == "__main__":
    sys.exit(main())
