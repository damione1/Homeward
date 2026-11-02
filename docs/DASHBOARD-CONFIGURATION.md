# Dashboard configuration qualification

The normal frontend consumes DashboardService List/Get/Watch. Production
registration lives in internal/server/dashboard_transport.go; the catalogue
comes from the exact SQL generation published by configactivation. The
requester never becomes a dashboard owner. Ownership is explicit in validated
homeward source metadata. Raw files remain on the separately authorized
Configuration API and are absent from Dashboard Get.

qualification/frontend/dashboard-widgets/a.yaml and b.yaml differ in actual
structure. B changes columns, moves the thermostat, binds it to the second
simultaneously observable Climate entity, removes A's agenda and room, and adds
a task list and another room. The two Climate sources share a schema but have
different observations, supplied in external devices.json. A local selection
of the first Climate source cannot pass this test.

Two real SQL lists imported from lists.json share an item UID while retaining
different summaries. A uses the first exact list ID with limit 1; B uses the
second ID with limit 2. A native checkbox mutation must complete B's item while
leaving A's equal UID unchanged. The importer uses the real list composition,
authorization and public ListService readback; no list RPC is replaced.

OWNER_ACCOUNT_ID and LIST_A_ID/LIST_B_ID are replaced by freshly created IDs before public
ConfigPlan/ConfigApply publication. No frontend source or Slint property
receives fixture values. The published source importer, production transport
and normal frontend decoder are the exercised path.

TestDashboardWidgetsProductionPublicationWatchAndSourcePrivacy checks real
TLS authentication, exact source publication, Watch resync, source privacy,
actual timestamps and another account's own-dashboard policy. The opt-in
TestDashboardWidgetsConfigurationServer starts fresh PostgreSQL, certificates
and accounts. Its only device adapter is a terminal fake driver, with no
physical provider URL or credential. Submitted requests retain their canonical
entity ID and arguments. Acknowledgement never synthesizes an observed effect.

Build the normal frontend, Go test server and existing native helper once
under frontend.py reservations. Run scripts/qualify-frontend-dashboard-config.py
with --server, --binary, --helper (absolute artifact paths) and --proof (a new
absolute directory). The script contains no build or ADB operation.

It authenticates through native controls, captures A and submits its native
thermostat step, publishes B and verifies that the next step targets the other
entity. A configured room action and native Back must restore its focus. It
checks the exact list source, row limit and checkbox write scope, omitted blocks
and conditional visibility for fresh true,
false and stale observations. PID and executable SHA remain constant. An
fresh public endpoint/preferences are installed only when the normal macOS
product files are absent and removed only if their exact owned JSON content remains.
The normal HOME preserves native keychain resolution; credentials use a unique
synthetic namespace that the runner removes. Existing user profiles are preserved.

--serve-only exposes the same fixture for separately coordinated Android
testing. Private fixture/ready.json contains the synthetic login and CA; do not
publish it. public-endpoint.json contains only origin and public certificate.
Android qualification requires a normal APK, native login, the same server
publications and preservation of app data/signing. A server-only run is
explicitly not a UI pass.

Controls are atomic command.json objects with strictly increasing sequence and
one action: apply-a, apply-b, apply-showcase, condition-on, condition-off, condition-stale or
snapshot. The fixture writes control-result.json, published.json, exact source
copies and wire-a.json/wire-b.json. Creating fixture/stop joins the server and
produces the command receipt. Retain result JSON, binary/source hashes, wire
snapshots, native trees and captures together. Review images before claiming
visual qualification. A stale executable, preview feature, Slint setter or
test-only replacement DashboardService does not qualify this contract.

showcase.yaml contains the complete tablet composition: overview clock/weather
and thermostat, a three-column room grid, agenda, task list, security and status
observations. It uses the same compiler, production API and normal renderer.
Its calendar source intentionally has no production CalendarService in this
fixture, so unavailable calendar data must remain honest; no invented event or
fallback calendar is permitted. --smoke-only verifies published wire structure
and exact bindings without claiming a native UI pass.
