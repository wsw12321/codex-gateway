#!/usr/bin/env python3
"""Verify the built-in reviewed API catalog and its local override contract."""

from decimal import Decimal
import json
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[2]
CATALOG = ROOT / "internal/config/data/pricing.json"
PRICE_FIELDS = (
    "input_usd_per_million", "output_usd_per_million", "cached_input_usd_per_million",
    "cache_write_5m_usd_per_million", "cache_write_1h_usd_per_million",
)
CLAUDE_MODELS = {
    "claude-fable-5-1": (1000000, 1000000, {"short": ("10", "50", "0.25", "12.50", "20")}),
    "claude-fable-5-2": (1000000, 1000000, {"short": ("8", "40", "0.2", "10", "16")}),
    "claude-opus-5-5": (1000000, 1000000, {"short": ("4", "20", "0.20", "5", "8")}),
    "claude-sonnet-5-5": (1000000, 1000000, {"short": ("2", "10", "0.10", "2.50", "4")}),
    "claude-haiku-4-5-20251001": (200000, 200000, {"short": ("1", "5", "0.10", "1.25", "2")}),
    "claude-haiku-5-5": (1000000, 100000, {
        "short": ("0.10", "0.50", "0.01", "0.125", "0.20"),
        "long": ("0.50", "2.50", "0.05", "0.625", "1"),
    }),
}


class PricingCatalogTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.catalog = json.loads(CATALOG.read_text())

    def test_claude_standard_prices_and_context_limits(self):
        self.assertEqual(self.catalog["catalog_as_of"], "2026-10-17")
        self.assertEqual(self.catalog["fx_as_of"], "2026-08-20")
        self.assertEqual(self.catalog["usd_cny_rate"], "7.20")
        self.assertEqual(self.catalog["fallback_policy"], {
            "unknown_service_tier": "max_published",
            "missing_price_combination": "max_published",
            "missing_cache_write_tokens": "all_uncached_as_write",
        })
        self.assertEqual({model for model in self.catalog["models"] if model.startswith("claude-")},
                         set(CLAUDE_MODELS))
        for model, (limit, threshold, contexts) in CLAUDE_MODELS.items():
            with self.subTest(model=model):
                self.assertEqual(self.catalog["models"][model], {
                    "cache_write_mode": "separate_by_ttl",
                    "max_input_tokens": limit,
                    "long_context_threshold_tokens": threshold,
                    "service_tiers": {"standard": {
                        context: dict(zip(PRICE_FIELDS, prices))
                        for context, prices in contexts.items()
                    }},
                })

    def test_revised_openai_prices_in_all_service_tiers(self):
        fields = ("input_usd_per_million", "cached_input_usd_per_million",
                  "cache_write_usd_per_million", "output_usd_per_million")
        standard_prices = {
            "gpt-6.2-astra": {"short": ("8", "0.4", "10", "40"),
                              "long": ("16", "0.8", "20", "60")},
            "gpt-6.2-sol": {"short": ("2", "0.1", "2.5", "10"),
                            "long": ("4", "0.2", "5", "15")},
            "gpt-6.1-terra": {"short": ("1", "0.05", "1.25", "5"),
                              "long": ("2", "0.1", "2.5", "7.5")},
            "gpt-6-sol": {"short": ("2", "0.2", "2.5", "10"),
                          "long": ("4", "0.4", "5", "15")},
            "gpt-6-luna": {"short": ("0.1", "0.01", "0.125", "0.5"),
                           "long": ("0.2", "0.02", "0.25", "0.75")},
            "gpt-5.6-sol": {"short": ("4", "0.4", "5", "20"),
                            "long": ("8", "0.8", "10", "30")},
        }
        for model, contexts in standard_prices.items():
            for tier, multiplier in (("standard", "1"), ("flex", "0.5"), ("fast", "2")):
                for context, prices in contexts.items():
                    with self.subTest(model=model, tier=tier, context=context):
                        actual = self.catalog["models"][model]["service_tiers"][tier][context]
                        self.assertEqual({field: Decimal(value) for field, value in actual.items()},
                                         {field: Decimal(value) * Decimal(multiplier)
                                          for field, value in zip(fields, prices)})

    def test_argon_current_official_api_price(self):
        rule = self.catalog["models"]["gemini-4-argon-high"]
        self.assertEqual(rule["service_tiers"], {"standard": {"short": {
            "input_usd_per_million": "2",
            "cached_input_usd_per_million": "0.10",
            "output_usd_per_million": "10",
        }}})

    def test_deployment_carries_no_pricing_copy(self):
        # The catalog is compiled into the Gateway; .env templates and Compose
        # must not carry a second copy that could drift from it.
        self.assertFalse((ROOT / "deploy/pricing-v2.example.json").exists())
        for name in ("env.example", "env.gpt-5.6.example"):
            with self.subTest(example=name):
                lines = (ROOT / "deploy" / name).read_text().splitlines()
                self.assertFalse([line for line in lines if line.startswith("GATEWAY_USAGE_PRICING_JSON=")])
        compose = (ROOT / "docker-compose.yml").read_text()
        self.assertNotIn("GATEWAY_USAGE_PRICING_JSON", compose)
        self.assertIn("      GATEWAY_USAGE_PRICING_OVERRIDE_FILE: /etc/codex-gateway/pricing/override.json\n", compose)
        self.assertIn("        source: ./deploy/pricing\n        target: /etc/codex-gateway/pricing\n"
                      "        read_only: true\n", compose)

    def test_override_example_changes_prices_of_built_in_models_only(self):
        override = json.loads((ROOT / "deploy/pricing/override.example.json").read_text())
        self.assertEqual(override["schema_version"], self.catalog["schema_version"])
        self.assertLessEqual(set(override), {"schema_version", "usd_cny_rate", "fx_as_of", "models"})
        for model, rule in override["models"].items():
            with self.subTest(model=model):
                built_in = self.catalog["models"][model]
                for field in ("cache_write_mode", "max_input_tokens", "long_context_threshold_tokens"):
                    self.assertEqual(rule[field], built_in[field])
                self.assertEqual({tier: {context: set(prices) for context, prices in contexts.items()}
                                  for tier, contexts in rule["service_tiers"].items()},
                                 {tier: {context: set(prices) for context, prices in contexts.items()}
                                  for tier, contexts in built_in["service_tiers"].items()})

    def test_retired_pricing_setting_stops_every_compose_call(self):
        settings = (ROOT / "scripts/egress-settings.compose.yml").read_text()
        self.assertIn("      GATEWAY_USAGE_PRICING_JSON: ${GATEWAY_USAGE_PRICING_JSON:-}\n", settings)
        validator = (ROOT / "scripts/validate-egress.py").read_text()
        self.assertIn('if env.get("GATEWAY_USAGE_PRICING_JSON", "").strip():', validator)
        self.assertIn(" GATEWAY_USAGE_PRICING_JSON ", (ROOT / "scripts/egress-common.sh").read_text())
        script = (ROOT / "scripts/validate-compose.sh").read_text()
        self.assertIn(".services.gateway.environment.GATEWAY_USAGE_PRICING_JSON == null", script)
        self.assertNotIn("pricing_validator", script)

    def test_local_override_is_never_committed_or_built_into_images(self):
        gitignore = (ROOT / ".gitignore").read_text().splitlines()
        self.assertIn("deploy/pricing/*", gitignore)
        self.assertIn("!deploy/pricing/README.md", gitignore)
        self.assertIn("!deploy/pricing/override.example.json", gitignore)
        self.assertNotIn("!deploy/pricing/override.json", gitignore)
        self.assertIn("deploy/pricing", (ROOT / ".dockerignore").read_text().splitlines())


if __name__ == "__main__":
    unittest.main()
