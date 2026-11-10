"""Regression policy checks, independent of machine speed or CI configuration."""

from pathlib import Path
import runpy
import unittest

compare = runpy.run_path(str(Path(__file__).with_name("benchmark-runtime")))["compare"]


class ComparisonTest(unittest.TestCase):
    def test_controls_and_slowdown(self):
        # Values are whole-job nanoseconds. A relative-only or absolute-only
        # detector would incorrectly reject one of these controls.
        for baseline, candidate, expected in (
            (100_000_000, 100_000_000, "pass"),
            (20_000_000, 40_000_000, "pass"),
            (1_000_000_000, 1_150_000_000, "pass"),
            (100_000_000, 140_000_000, "regression"),
            (140_000_000, 100_000_000, "pass"),
        ):
            with self.subTest(baseline=baseline, candidate=candidate):
                pairs = [{"baseline": baseline, "candidate": candidate}] * 12
                self.assertEqual(compare(pairs)["status"], expected)

    def test_outliers_and_uncertainty(self):
        # One outlier is tolerated on either side of the budget. Mixed evidence
        # must not pass merely because its median is below the budget.
        for over, expected in ((0, "pass"), (1, "pass"), (2, "inconclusive"),
                               (6, "inconclusive"), (10, "inconclusive"),
                               (11, "regression"), (12, "regression")):
            with self.subTest(over=over):
                pairs = ([{"baseline": 100_000_000, "candidate": 900_000_000}] * over
                         + [{"baseline": 100_000_000, "candidate": 100_000_000}] * (12 - over))
                self.assertEqual(compare(pairs)["status"], expected)

    def test_shared_drift_and_unpaired_noise(self):
        # Large common host movement does not make a consistent paired result
        # uncertain, and must not hide a slowdown present throughout that drift.
        for added, expected in ((0, "pass"), (40_000_000, "regression")):
            pairs = [{"baseline": n, "candidate": n + added}
                     for n in [50_000_000, 150_000_000] * 6]
            with self.subTest(added=added):
                self.assertEqual(compare(pairs)["status"], expected)
        # Same marginal distributions, but opposing movement within each pair:
        # half the comparisons exceed the budget. This really is uncertain.
        pairs = [{"baseline": n, "candidate": 200_000_000 - n}
                 for n in [50_000_000, 150_000_000] * 6]
        self.assertEqual(compare(pairs)["status"], "inconclusive")

    def test_invalid_evidence(self):
        for pairs in ([], [{"baseline": 1, "candidate": float("nan")}] * 12,
                      [{"baseline": 0, "candidate": 1}] * 12):
            with self.subTest(pairs=pairs), self.assertRaises(ValueError):
                compare(pairs)


if __name__ == "__main__":
    unittest.main()
