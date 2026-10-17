# Frozen product scope — NHA-002

Documentary extraction dated 2026-10-17, Python 3.12.8; no HA or device access.
`manifest.json`: exactly 850 unique IDs from 38 source labels, including “hors registre”.
Sorted ID encoding: UTF-8, one ID per line, LF final. SHA-256: `4cdcd750642c903a58195e404717042b9c7e2fa045799132f38f73291e069d31`.
19 `diagnostic_open` flags are independent of `driver_support: unqualified`.
No supported driver or resolved diagnosis is claimed. `logic.json` has 89 pending objects,
all explicitly inactive (50 automations, 22 scripts, 17 scenes).

Source of truth: vault `Projects/New Home Assistant/Sélection des entités.md`;
independent cross-check `Architecture/Couverture des 850 entités.md`;
diagnostics `Entités à vérifier.md`. Source bytes are hashed in `sources/provenance.json`.
Only decision/ID/source columns and diagnostic IDs are exported; no states,
coordinates, images, URLs, credentials, personal event payloads or device addresses.
The source extracts permit checks in a clone without access to the private vault.

Run `python3 scripts/check-scope.py` and `python3 scripts/test-scope.py`.
For original-document equality: `python3 scripts/check-scope.py --vault '/Users/damien/Projects/notes/Projects/New Home Assistant'`.
Actual results: exact equality and hashes passed; five tests passed, including excluded
`ai_task.ollama_ai_task` substitution naming the added/missing IDs, duplicate,
diagnostic deletion and unsolicited logic activation. No automatic activation API is provided.
Unknown candidates fail closed with a reason; rediscovery cannot authorize one.
