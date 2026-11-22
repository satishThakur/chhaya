"""Garmin Connect: sign in once with a password, then reuse the saved tokens."""
import logging
import os

log = logging.getLogger("garmin_sync")


def login(email, password, tokendir, garmin_cls=None):
    """Use the saved tokens if they work; otherwise sign in with the password and save them.
    Garmin rate-limits password logins, so a pass must not log in again every time. The password is
    never logged."""
    if garmin_cls is None:
        from garminconnect import Garmin as garmin_cls
    try:
        client = garmin_cls()
        client.login(tokendir)
        log.info("signed in with saved tokens")
        return client
    except Exception as e:  # tokens missing, expired or refused: fall back to the password
        log.info("saved tokens not usable (%s); signing in with the password", type(e).__name__)
    client = garmin_cls(email=email, password=password)
    client.login()
    os.makedirs(tokendir, mode=0o700, exist_ok=True)
    client.garth.dump(tokendir)
    log.info("signed in with the password; tokens saved")
    return client


def fetch(client, date):
    """Raw responses for one day, one per section. A failing call is returned as the exception,
    never turned into 'no data', so the caller can say which section broke."""
    calls = {
        "sleep": lambda: client.get_sleep_data(date),
        "hrv": lambda: client.get_hrv_data(date),
        "stress": lambda: client.get_stress_data(date),
        "daily_stats": lambda: client.get_stats(date),
        "body_battery": lambda: client.get_body_battery(date, date),
        "activities": lambda: client.get_activities_by_date(date, date),
    }
    out = {}
    for name, call in calls.items():
        try:
            out[name] = call()
        except Exception as e:
            out[name] = e
    return out
