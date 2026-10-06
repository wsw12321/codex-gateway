"use strict";

const clone = (value) => JSON.parse(JSON.stringify(value));
const tokenPrices = (input, cached, write, output) => ({input_usd_per_million: input,
  cached_input_usd_per_million: cached, ...(write == null ? {} : {cache_write_usd_per_million: write}), output_usd_per_million: output});
const row = (model, price, extra = {}) => ({model, schema_version: price.service_tiers ? 2 : 1,
  configured_price: clone(price), effective_price: clone(price), multiplier: "1", version: 0,
  structure_id: `structure-${model}`, source: "config", conflict: false, editable: true, updated_at: null, ...extra});
const rows = () => [
  row("gpt-example", {cache_write_mode: "separate", max_input_tokens: 1048576, long_context_threshold_tokens: 200000,
    service_tiers: {
      standard: {short: tokenPrices("2.5", "0.25", "3.125", "10"), long: tokenPrices("5", "0.5", "6.25", "15")},
      fast: {short: tokenPrices("5", "0.5", "6.25", "20"), long: tokenPrices("10", "1", "12.5", "30")},
      flex: {short: tokenPrices("1.25", "0.125", "1.5625", "5"), long: tokenPrices("2.5", "0.25", "3.125", "7.5")},
    }}, {multiplier: "0.8", updated_at: "2026-10-06T08:30:00Z", source: "override", version: 4}),
  row("gemini-example", {cache_write_mode: "included_in_input", max_input_tokens: 1048576, long_context_threshold_tokens: 200000,
    service_tiers: {standard: {short: tokenPrices("1.25", "0.125", null, "10"), long: tokenPrices("2.5", "0.25", null, "15")}}}),
  row("legacy-example", tokenPrices("0.15", "0.015", null, "0.6")),
  row("codex-auto-review", tokenPrices("0", "0", null, "0"), {source: "internal", editable: false}),
];
module.exports = {rows, clone, tokenPrices, row};
