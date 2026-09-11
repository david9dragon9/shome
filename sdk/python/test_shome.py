"""Unit tests for the parts of the SDK that need no controller."""
import sys, os, unittest
sys.path.insert(0, os.path.dirname(__file__))
from shome import _parse_size, _parse_duration_ns, _job, Job


class TestParseSize(unittest.TestCase):
    def test_bare_number_is_mib_like_slurm(self):
        # Slurm's --mem default unit is megabytes; matching it avoids a
        # thousand-fold surprise for anyone porting a script.
        self.assertEqual(_parse_size("1024"), 1024 << 20)
        self.assertEqual(_parse_size(512), 512 << 20)

    def test_suffixes(self):
        self.assertEqual(_parse_size("4G"), 4 << 30)
        self.assertEqual(_parse_size("512M"), 512 << 20)
        self.assertEqual(_parse_size("2T"), 2 << 40)
        self.assertEqual(_parse_size("2048K"), 2048 << 10)

    def test_fractional_and_lowercase(self):
        self.assertEqual(_parse_size("1.5g"), int(1.5 * (1 << 30)))

    def test_empty(self):
        self.assertEqual(_parse_size(""), 0)


class TestParseDuration(unittest.TestCase):
    def test_slurm_forms(self):
        ns = 1_000_000_000
        self.assertEqual(_parse_duration_ns("30"), 30 * 60 * ns)          # MM
        self.assertEqual(_parse_duration_ns("1:30"), 90 * ns)             # MM:SS
        self.assertEqual(_parse_duration_ns("02:30:00"), 9000 * ns)       # HH:MM:SS
        self.assertEqual(_parse_duration_ns("1-00"), 86400 * ns)          # D-HH
        self.assertEqual(_parse_duration_ns("1-12:30:00"),
                         (86400 + 12 * 3600 + 1800) * ns)


class TestJobModel(unittest.TestCase):
    def test_terminal_states(self):
        for st in ("COMPLETED", "FAILED", "CANCELLED", "TIMEOUT", "OUT_OF_MEMORY"):
            self.assertTrue(_job({"state": st}).finished, st)
        for st in ("PENDING", "RUNNING"):
            self.assertFalse(_job({"state": st}).finished, st)

    def test_ok_only_for_completed(self):
        self.assertTrue(_job({"state": "COMPLETED"}).ok)
        self.assertFalse(_job({"state": "FAILED"}).ok)

    def test_missing_fields_are_safe(self):
        j = _job(None)
        self.assertEqual(j.id, 0)
        self.assertIsInstance(j, Job)


if __name__ == "__main__":
    unittest.main(verbosity=2)
