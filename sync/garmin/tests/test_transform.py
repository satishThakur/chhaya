import unittest

from garmin_sync import transform as t


class Sleep(unittest.TestCase):
    RAW = {"dailySleepDTO": {"sleepTimeSeconds": 27000, "deepSleepSeconds": 5400, "lightSleepSeconds": 14400,
                             "remSleepSeconds": 5400, "awakeSleepSeconds": 1800,
                             "sleepScores": {"overall": {"value": 81}}, "avgOvernightHrv": 52,
                             "sleepEndTimestampGMT": 1795221600000}}

    def test_maps_the_fields_and_converts_the_timestamp_in_utc(self):
        out = t.sleep(self.RAW)
        self.assertEqual(out["total_seconds"], 27000.0)
        self.assertEqual(out["score"], 81.0)
        self.assertEqual(out["avg_hrv"], 52.0)
        # 1795221600000 ms is 2026-11-21T00:40:00Z whatever the machine's time zone is
        self.assertEqual(out["ended_at"], "2026-11-21T00:40:00Z")

    def test_no_recorded_sleep_is_no_data_not_zero(self):
        self.assertIsNone(t.sleep({"dailySleepDTO": {"sleepTimeSeconds": 0}}))
        self.assertIsNone(t.sleep({"dailySleepDTO": {}}))
        self.assertIsNone(t.sleep({}))
        self.assertIsNone(t.sleep(None))

    def test_a_missing_score_is_left_out(self):
        raw = {"dailySleepDTO": {"sleepTimeSeconds": 100, "sleepScores": {"overall": {}}}}
        self.assertNotIn("score", t.sleep(raw))


class Hrv(unittest.TestCase):
    def test_reads_the_summary_where_garmin_puts_it_and_the_flat_shape_too(self):
        self.assertEqual(t.hrv({"hrvSummary": {"lastNightAvg": 51, "weeklyAvg": 49, "status": "BALANCED"}}),
                         {"last_night_avg": 51.0, "weekly_avg": 49.0})
        self.assertEqual(t.hrv({"lastNightAvg": 51}), {"last_night_avg": 51.0})

    def test_nothing_to_report_is_none(self):
        self.assertIsNone(t.hrv({"hrvSummary": {"status": "NONE"}}))
        self.assertIsNone(t.hrv(None))


class Stress(unittest.TestCase):
    def test_garmins_minus_one_means_no_data(self):
        out = t.stress({"avgStressLevel": -1, "maxStressLevel": 91, "restStressDuration": 21000})
        self.assertEqual(out, {"max": 91.0, "rest_seconds": 21000.0})

    def test_all_sentinels_is_none(self):
        self.assertIsNone(t.stress({"avgStressLevel": -1, "maxStressLevel": -2}))


class DailyStats(unittest.TestCase):
    def test_zero_steps_is_a_measurement_but_zero_heart_rate_is_not(self):
        out = t.daily_stats({"totalSteps": 0, "restingHeartRate": 0, "maxHeartRate": 151})
        self.assertEqual(out["steps"], 0.0)
        self.assertNotIn("hr_resting", out)
        self.assertEqual(out["hr_max"], 151.0)

    def test_maps_every_field(self):
        raw = {"totalSteps": 9120, "totalKilocalories": 2410, "activeKilocalories": 610, "bmrKilocalories": 1800,
               "totalDistanceMeters": 7100, "restingHeartRate": 54, "minHeartRate": 47, "maxHeartRate": 151,
               "moderateIntensityMinutes": 22, "vigorousIntensityMinutes": 8}
        self.assertEqual(len(t.daily_stats(raw)), 10)


class BodyBattery(unittest.TestCase):
    def test_high_and_low_come_from_the_values_array(self):
        raw = [{"charged": 62, "drained": 58, "bodyBatteryValuesArray": [[1, 40], [2, 91], [3, None], [4, 17]]}]
        out = t.body_battery(raw)
        self.assertEqual((out["charged"], out["drained"], out["high"], out["low"]), (62.0, 58.0, 91.0, 17.0))

    def test_falls_back_to_highest_lowest_keys(self):
        self.assertEqual(t.body_battery([{"highest": 80, "lowest": 20}]), {"high": 80.0, "low": 20.0})

    def test_empty_is_none(self):
        self.assertIsNone(t.body_battery([]))
        self.assertIsNone(t.body_battery(None))


class Activities(unittest.TestCase):
    def test_start_time_as_a_gmt_string_or_epoch_ms(self):
        a = t.activities([
            {"activityType": {"typeKey": "running"}, "startTimeGMT": "2026-11-21 00:30:00", "duration": 3000.5, "distance": 10000,
             "calories": 640, "averageHR": 148, "maxHR": 171},
            {"activityType": {"typeKey": "walking"}, "startTimeGMT": 1795221600000, "duration": 600},
        ])
        self.assertEqual(a[0]["start_time"], "2026-11-21T00:30:00Z")
        self.assertEqual(a[0]["avg_hr"], 148.0)
        self.assertEqual(a[1]["start_time"], "2026-11-21T00:40:00Z")
        self.assertNotIn("avg_hr", a[1])

    def test_an_activity_without_a_type_start_or_duration_is_dropped(self):
        self.assertEqual(t.activities([{"startTimeGMT": "2026-11-21 00:30:00", "duration": 5},
                                       {"activityType": {"typeKey": "x"}, "duration": 5},
                                       {"activityType": {"typeKey": "x"}, "startTimeGMT": "2026-11-21 00:30:00", "duration": 0}]), [])
        self.assertEqual(t.activities(None), [])


class Document(unittest.TestCase):
    def test_empty_sections_are_left_out_and_an_empty_day_is_empty(self):
        doc = t.day_document("2026-11-21", {"sleep": None, "hrv": {}, "stress": {"avg": 3.0}, "activities": []})
        self.assertEqual(doc, {"date": "2026-11-21", "stress": {"avg": 3.0}})
        self.assertTrue(t.is_empty(t.day_document("2026-11-21", {"sleep": None})))


if __name__ == "__main__":
    unittest.main()
