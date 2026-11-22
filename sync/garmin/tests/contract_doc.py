"""Prints the day document the sidecar would post for a realistic set of Garmin responses.
The Go test internal/health/contract_test.go feeds it to the gateway's strict decoder, so a field
the sidecar sends and the gateway does not know (the old pipeline's HRV bug) fails a test."""
import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from garmin_sync import main  # noqa: E402

RAW = {
    "sleep": {"dailySleepDTO": {"sleepTimeSeconds": 27000, "deepSleepSeconds": 5400, "lightSleepSeconds": 14400,
                                "remSleepSeconds": 5400, "awakeSleepSeconds": 1800, "sleepScores": {"overall": {"value": 81}},
                                "avgOvernightHrv": 52, "sleepEndTimestampGMT": 1795221600000}},
    "hrv": {"hrvSummary": {"lastNightAvg": 51, "weeklyAvg": 49, "status": "BALANCED"}},
    "stress": {"avgStressLevel": 28, "maxStressLevel": 91, "restStressDuration": 21000},
    "daily_stats": {"totalSteps": 9120, "totalKilocalories": 2410, "activeKilocalories": 610, "bmrKilocalories": 1800,
                    "totalDistanceMeters": 7100, "restingHeartRate": 54, "minHeartRate": 47, "maxHeartRate": 151,
                    "moderateIntensityMinutes": 22, "vigorousIntensityMinutes": 8},
    "body_battery": [{"charged": 62, "drained": 58, "bodyBatteryValuesArray": [[1, 40], [2, 91], [3, 17]]}],
    "activities": [{"activityType": {"typeKey": "running"}, "startTimeGMT": "2026-11-21 00:30:00", "duration": 3000.5,
                    "distance": 10000.0, "calories": 640, "averageHR": 148, "maxHR": 171}],
}

doc, errors = main.build_day("2026-11-21", RAW)
assert not errors, errors
print(json.dumps(doc))
