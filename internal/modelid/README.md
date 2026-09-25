# Model identification reference data

This package uses WhatsMyLLM's published bank v2026.09.5, fixed English prose
challenge set `environment-05`, gates v2026.09.1, and verdict v2026.09.1.
`environment-05` measured 13 correct results in 14 same-model trials in the
published challenge table. The bank contains 21 candidates. The gateway does
not update these assets at runtime. The package checks the exact SHA-256 of
each embedded file at startup and fails closed if an asset changes.

| Asset | Original URL | SHA-256 |
| --- | --- | --- |
| `data/bank.json` | https://whatsmyllm.com/data/bank/v2026.09.5.json | `d21c1413eeac2f5238f37bc97df0299591433c14a1bbd5534546e95697fbd11f` |
| `data/challenges.json` | https://whatsmyllm.com/data/challenges.json | `dd3772110658f47ce066c4770feea28254c88aad1047791da2b7a27b9a9b671d` |
| `data/gates.json` | https://whatsmyllm.com/data/gates.json | `4a3ad9dea0a63edbf7735e0cced5643662c6ceadeb42db8f52c65b12a6b5705c` |
| `data/verdict.json` | https://whatsmyllm.com/data/verdict.json | `26ee5b482b73fc93e75b3b43aa553ffca3992ffbcf7885e97f5b98be71eaca52` |
| `testdata/example.json` | https://whatsmyllm.com/data/example.json | `7028ec6fdb9a878da4d0a592c6f673aca35c6ed0a3d5b25c8ee7751ea4f72653` |

The Go scorer ports these published browser modules: [core](https://whatsmyllm.com/js/fingerprint-core.js),
[gates](https://whatsmyllm.com/js/gates.js), and
[verdict](https://whatsmyllm.com/js/verdict.js). It follows the page's
plain-reply input checks and requires all three probe answers to be usable.
The official recorded enrollment example is kept under `testdata` solely to
verify candidate order, fit, margin, and verdict parity. Run results must
never persist raw replies or reply digests.

The [WhatsMyLLM method](https://whatsmyllm.com/methodology/) is a statistical
comparison; neither a clear match nor a weak match proves the actual serving
model or provider. The source site credits the underlying method, scoring core,
challenge templates, and 16 bank entries to the MIT-licensed
[ModelTrace](https://github.com/xqy2006/ModelTrace). Its own gates, verdict,
and additional fingerprints are credited to WhatsMyLLM. See the root
`THIRD_PARTY_NOTICES` for the ModelTrace license text.
