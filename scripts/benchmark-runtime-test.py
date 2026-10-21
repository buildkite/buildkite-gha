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
        pairs = [{"baseline": 100_000_000, "candidate": 100_000_000}] * 12
        pairs[0] = {"baseline": 100_000_000, "candidate": 900_000_000}
        self.assertEqual(compare(pairs)["status"], "pass")
        # Ten consistent slow pairs are not enough to call a regression, but
        # the over-budget median must not silently pass either.
        pairs[:10] = [{"baseline": 100_000_000, "candidate": 140_000_000}] * 10
        self.assertEqual(compare(pairs)["status"], "inconclusive")
        pairs[10] = {"baseline": 100_000_000, "candidate": 140_000_000}
        self.assertEqual(compare(pairs)["status"], "regression")
        # Even matching medians cannot certify an unstable machine.
        pairs = [{"baseline": n, "candidate": n} for n in [50_000_000, 150_000_000] * 6]
        self.assertEqual(compare(pairs)["status"], "inconclusive")

    def test_invalid_evidence(self):
        for pairs in ([], [{"baseline": 1, "candidate": float("nan")}] * 12,
                      [{"baseline": 0, "candidate": 1}] * 12):
            with self.subTest(pairs=pairs), self.assertRaises(ValueError):
                compare(pairs)


if __name__ == "__main__":
    unittest.main()
