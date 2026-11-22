"""Garmin's raw responses -> the day document the gateway's /ingest/day accepts.

Pure functions, no network. Garmin's responses are undocumented and have changed shape over time,
so each reader accepts the shapes seen so far, and a value that is missing, null or Garmin's
"no data" sentinel (a negative number) is left OUT of the document. The gateway treats an absent
field as absent and a present one as a measurement, so sending a 0 for "no reading" would put a
false zero into every average.
"""
from datetime import datetime, timezone


def _num(v):
    """A real, non-negative number or None. Garmin uses -1 / -2 for "no data"."""
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        return None
    return float(v) if v >= 0 else None


def _put(out, key, v, positive=False):
    n = _num(v)
    if n is not None and (n > 0 or not positive):
        out[key] = n


def _iso(ts):
    """A Garmin timestamp -> RFC 3339 UTC, or None. Accepts epoch milliseconds or
    "YYYY-MM-DD HH:MM:SS" / ISO strings, which Garmin sends in GMT."""
    if isinstance(ts, (int, float)) and not isinstance(ts, bool):
        return datetime.fromtimestamp(ts / 1000, tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    if isinstance(ts, str) and ts:
        s = ts.replace(" ", "T").rstrip("Z")
        try:
            dt = datetime.fromisoformat(s)
        except ValueError:
            return None
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    return None


def sleep(raw):
    dto = (raw or {}).get("dailySleepDTO") or {}
    out = {}
    _put(out, "total_seconds", dto.get("sleepTimeSeconds"), positive=True)
    if "total_seconds" not in out:
        return None  # no sleep was recorded for this night
    _put(out, "deep_seconds", dto.get("deepSleepSeconds"))
    _put(out, "light_seconds", dto.get("lightSleepSeconds"))
    _put(out, "rem_seconds", dto.get("remSleepSeconds"))
    _put(out, "awake_seconds", dto.get("awakeSleepSeconds"))
    score = ((dto.get("sleepScores") or {}).get("overall") or {}).get("value")
    _put(out, "score", score, positive=True)
    hrv = dto.get("avgOvernightHrv", dto.get("averageHRV"))
    _put(out, "avg_hrv", hrv, positive=True)
    end = _iso(dto.get("sleepEndTimestampGMT"))
    if end:
        out["ended_at"] = end
    return out


def hrv(raw):
    summary = (raw or {}).get("hrvSummary") or raw or {}
    out = {}
    _put(out, "last_night_avg", summary.get("lastNightAvg"), positive=True)
    _put(out, "weekly_avg", summary.get("weeklyAvg"), positive=True)
    return out or None


def stress(raw):
    raw = raw or {}
    out = {}
    _put(out, "avg", raw.get("avgStressLevel"), positive=True)
    _put(out, "max", raw.get("maxStressLevel"), positive=True)
    _put(out, "rest_seconds", raw.get("restStressDuration"))
    return out or None


def daily_stats(raw):
    raw = raw or {}
    out = {}
    for ours, theirs in (
        ("steps", "totalSteps"),
        ("calories_total", "totalKilocalories"),
        ("calories_active", "activeKilocalories"),
        ("calories_bmr", "bmrKilocalories"),
        ("distance_meters", "totalDistanceMeters"),
        ("intensity_moderate_minutes", "moderateIntensityMinutes"),
        ("intensity_vigorous_minutes", "vigorousIntensityMinutes"),
    ):
        _put(out, ours, raw.get(theirs))
    for ours, theirs in (("hr_resting", "restingHeartRate"), ("hr_min", "minHeartRate"), ("hr_max", "maxHeartRate")):
        _put(out, ours, raw.get(theirs), positive=True)
    return out or None


def body_battery(raw):
    day = raw[0] if isinstance(raw, list) and raw else raw
    if not isinstance(day, dict):
        return None
    out = {}
    _put(out, "charged", day.get("charged"))
    _put(out, "drained", day.get("drained"))
    values = [p[1] for p in (day.get("bodyBatteryValuesArray") or []) if isinstance(p, (list, tuple)) and len(p) > 1 and _num(p[1]) is not None]
    if values:
        out["high"], out["low"] = float(max(values)), float(min(values))
    else:
        _put(out, "high", day.get("highest"))
        _put(out, "low", day.get("lowest"))
    return out or None


def activities(raw):
    out = []
    for a in raw or []:
        start = _iso(a.get("startTimeGMT"))
        duration = _num(a.get("duration"))
        kind = (a.get("activityType") or {}).get("typeKey")
        if not (start and duration and duration > 0 and kind):
            continue
        item = {"type": kind, "start_time": start, "duration_seconds": duration}
        _put(item, "distance_meters", a.get("distance"))
        _put(item, "calories", a.get("calories"))
        _put(item, "avg_hr", a.get("averageHR"), positive=True)
        _put(item, "max_hr", a.get("maxHR"), positive=True)
        out.append(item)
    return out


def day_document(date, parts):
    """parts maps section name -> its transformed value (None or empty means no data)."""
    doc = {"date": date}
    for key, val in parts.items():
        if val:
            doc[key] = val
    return doc


def is_empty(doc):
    return len(doc) <= 1
