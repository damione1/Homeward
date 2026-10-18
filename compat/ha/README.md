# HA documentary corpus — NHA-003

Reference profile: `ha-2026.10/core-1`, upstream Core 2026.10.1.
Prepared 2026-10-17 within the two-day limit. No production access.

`catalogue.json` records actual textual observations, source note/section/line and source hashes.
It classifies triggers, modes, flow control, scenes, script calls, blueprints, includes and template
functions/filters in the four real inventory notes. Counts are textual occurrences, not AST coverage,
run counts, support or activation claims. Unused blueprints remain outside activation scope.

All source examples are documentary only. Redactions and normalized `!input`/`!include`
mappings are explicitly `non_executable`; original notes are not copied into runnable inputs.
Synthetic structural replacements: restart→01, queued→02, waits→03,
MQTT blueprint action input→04, snapshots→05, dates→06, state→07,
for/repeat timing→08, script blueprint/calls→09, template defaults→10.
Secrets/media redactions do not get executable equivalents: the fixture replaces the
side effect with a local helper. No HomeKit, Cast or other excluded route is reintroduced.

Each `.yaml` is a complete HA package encoded as JSON (a YAML subset), containing only
synthetic helpers/events/actions. Each `.case.json` holds clock/event inputs and expected traces,
separate from importable YAML. MQTT requires a fake isolated broker; clock/state injection requires
the future isolated upstream test harness. Dates require its timezone setting. These are executable
configuration candidates, not evidence of upstream schema validation.

`python3 scripts/check-corpus.py` validates catalogue provenance shape, 10 package/case pairs,
privacy and a narrow deterministic Python oracle over the synthetic inputs/configuration.
The oracle is a test specification, not the Go engine or Home Assistant. It checks restart cancellation,
queued FIFO/max, release before timeout, duplicate MQTT events, snapshot restore, local calendar days
across DST, attribute-only state, cancelled for-timer, awaited/detached calls and float default.
A deliberately corrupted expected trace must fail. The script never connects to devices or a broker.
No differential HA validation has run; every case says `ha_reference_executed: false`.

Next gate, outside this ticket: isolated Core 2026.10.1 with upstream async/time utilities and fake
integrations, then Go runtime on the same input; normalize run IDs and compare causal traces.
Boundary-at-timeout ordering, dynamic template dependencies, reconnect/retained policies, parallel
branches and crash recovery remain additional gates. These fixtures activate none of the 89 real objects.
