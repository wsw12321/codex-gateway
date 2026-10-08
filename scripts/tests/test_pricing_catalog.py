#!/usr/bin/env python3
"""Verify the reviewed Claude catalog and production price validation."""

import copy
import json
from pathlib import Path
import shutil
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[2]
PRICE_FIELDS = (
    "input_usd_per_million", "output_usd_per_million", "cached_input_usd_per_million",
    "cache_write_5m_usd_per_million", "cache_write_1h_usd_per_million",
)
CLAUDE_MODELS = {
    "claude-fable-5-1": (1000000, ("10", "50", "0.25", "12.50", "20")),
    "claude-opus-5-5": (1000000, ("4", "20", "0.20", "5", "8")),
    "claude-sonnet-5-5": (1000000, ("2", "10", "0.10", "2.50", "4")),
    "claude-haiku-4-5-20251001": (200000, ("1", "5", "0.10", "1.25", "2")),
}


class PricingCatalogTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.catalog = json.loads((ROOT / "deploy/pricing-v2.example.json").read_text())

    def test_claude_standard_prices_and_context_limits(self):
        self.assertEqual(self.catalog["catalog_as_of"], "2026-10-08")
        self.assertEqual(self.catalog["fx_as_of"], "2026-08-20")
        self.assertEqual(self.catalog["usd_cny_rate"], "7.20")
        self.assertEqual(self.catalog["fallback_policy"], {
            "unknown_service_tier": "max_published",
            "missing_price_combination": "max_published",
            "missing_cache_write_tokens": "all_uncached_as_write",
        })
        self.assertEqual({model for model in self.catalog["models"] if model.startswith("claude-")},
                         set(CLAUDE_MODELS))
        for model, (limit, prices) in CLAUDE_MODELS.items():
            with self.subTest(model=model):
                self.assertEqual(self.catalog["models"][model], {
                    "cache_write_mode": "separate_by_ttl",
                    "max_input_tokens": limit,
                    "long_context_threshold_tokens": limit,
                    "service_tiers": {"standard": {"short": dict(zip(PRICE_FIELDS, prices))}},
                })

    def test_environment_examples_match_canonical_catalog(self):
        for name in ("env.example", "env.gpt-5.6.example"):
            with self.subTest(example=name):
                values = [line.split("=", 1)[1]
                          for line in (ROOT / "deploy" / name).read_text().splitlines()
                          if line.startswith("GATEWAY_USAGE_PRICING_JSON=")]
                self.assertEqual(len(values), 1)
                self.assertEqual(json.loads(values[0]), self.catalog)


@unittest.skipUnless(shutil.which("jq"), "jq is required for the production pricing validator")
class PricingValidatorTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.catalog = json.loads((ROOT / "deploy/pricing-v2.example.json").read_text())
        script = (ROOT / "scripts/validate-compose.sh").read_text()
        cls.validator = script.split("pricing_validator='\n", 1)[1].split("\n'\n", 1)[0]

    def validate(self, catalog):
        result = subprocess.run(
            ["jq", "-e", self.validator], input=json.dumps(catalog),
            capture_output=True, text=True, timeout=5,
        )
        self.assertNotIn("compile error", result.stderr)
        return result.returncode == 0

    def test_valid_catalog_and_custom_positive_claude_price(self):
        self.assertTrue(self.validate(self.catalog))
        changed = copy.deepcopy(self.catalog)
        changed["models"]["claude-fable-5-1"]["service_tiers"]["standard"]["short"].update(
            cache_write_1h_usd_per_million="21.50")
        self.assertTrue(self.validate(changed))

    def test_claude_requires_all_five_positive_decimal_prices(self):
        for field in PRICE_FIELDS:
            for value in (None, True, 1, -1, "", "0", "-1", "1e2", "NaN", "Infinity",
                          "0.1.2", "1000000000.1", "1000000001", "1" * 41):
                with self.subTest(field=field, value=value):
                    changed = copy.deepcopy(self.catalog)
                    price = changed["models"]["claude-opus-5-5"]["service_tiers"]["standard"]["short"]
                    price[field] = value
                    self.assertFalse(self.validate(changed))
            with self.subTest(missing=field):
                changed = copy.deepcopy(self.catalog)
                del changed["models"]["claude-opus-5-5"]["service_tiers"]["standard"]["short"][field]
                self.assertFalse(self.validate(changed))

    def test_claude_model_shapes_and_ids_are_strict(self):
        for model in CLAUDE_MODELS:
            mutations = {
                "missing model": lambda p: p["models"].pop(model),
                "legacy write price": lambda p: p["models"][model]["service_tiers"]["standard"]["short"].update(
                    cache_write_usd_per_million="2"),
                "legacy write mode": lambda p: p["models"][model].update(cache_write_mode="separate"),
                "wrong input limit": lambda p: p["models"][model].update(max_input_tokens=272000),
                "wrong boundary": lambda p: p["models"][model].update(long_context_threshold_tokens=272000),
                "Fast tier": lambda p: p["models"][model]["service_tiers"].update(
                    fast=p["models"][model]["service_tiers"]["standard"]),
                "long context price": lambda p: p["models"][model]["service_tiers"]["standard"].update(
                    long=p["models"][model]["service_tiers"]["standard"]["short"]),
            }
            for name, mutate in mutations.items():
                with self.subTest(model=model, mutation=name):
                    changed = copy.deepcopy(self.catalog)
                    mutate(changed)
                    self.assertFalse(self.validate(changed))
        for model in ("claude-haiku-4-5", "claude-haiku-5-5"):
            with self.subTest(unreviewed_model=model):
                changed = copy.deepcopy(self.catalog)
                changed["models"][model] = changed["models"]["claude-haiku-4-5-20251001"]
                self.assertFalse(self.validate(changed))


if __name__ == "__main__":
    unittest.main()
