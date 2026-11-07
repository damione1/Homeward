# Homeward

A local-first home automation platform built around a modular Go server.

Homeward aims to provide dependable home control, Home Assistant configuration compatibility,
and an open integration ecosystem. Embedded Go integrations are preferred, with optional
external integrations through a versioned Protobuf/gRPC contract.

The server and native Rust/Slint client are under active development.

Development documentation is maintained in the project's Obsidian vault.

## License

[GNU General Public License v3.0](LICENSE). Third-party dependencies retain their respective licenses.

## Contribute

Homeward is still young and changing quickly. The priority is to reach a stable foundation as soon as possible, so we aren't accepting external contributions or pull requests for now. Coordinating and reviewing them at this stage would slow that work.

Thank you for wanting to contribute. [Issues](https://github.com/damione1/Homeward/issues) and [discussions](https://github.com/damione1/Homeward/discussions) are welcome for bug reports, questions and feedback. We'll reopen contributions once homeward reaches the level of stability we're aiming for.

## Native development evaluation

The frontend consumes Slint Kit 0.7.1 and a matching local Slint Kit Pro checkout.
Configure both source trees before Cargo loads the workspace. The sibling
`slint-kit` alias keeps the runtime and build crates on one canonical source.
Each window initializes the shared services once with
`slint_kit_pro::install(&window)`; UI imports use `@kit` and `@kit-pro`.

```sh
python3 scripts/frontend.py configure --free-kit /path/to/slint-kit --pro-kit /path/to/slint-kit-pro --backend-cache /path/to/shared/homeward-cache
python3 scripts/frontend.py run -- brew install --cask android-commandlinetools
python3 scripts/frontend.py run --reserve-gb 14 -- python3 scripts/setup-frontend-android.py
python3 scripts/frontend.py run -- cargo install cargo-apk --version 0.10.0 --locked --root .tools
python3 scripts/frontend.py run -- python3 scripts/setup-frontend-transport.py
make frontend-desktop
make frontend-android
make qualify-frontend-transport
# Export the frozen NHA-036/NHA-055 migration artifacts documented in Obsidian first.
python3 scripts/frontend.py run -- make check
```

Configuration and kit links stay local. The runner reuses `.tools` and the shared Cargo
output, reserves the existing backend build locks, records source manifests and checks
the frontend's advisory 60 GB storage target before, during and after each round.
It retains at least 50 GB free under the current shared-host policy. The Android SDK and existing Java installation are configured
for this Homebrew development host; see the Obsidian ticket for the evaluated versions.

For Android updates, keep the development signing identity outside disposable caches
and reuse it across worktrees. Pass `--android-debug-keystore /absolute/path/to/debug.keystore`
to `configure`, or set `android_debug_keystore` in an existing untracked
`frontend.local.json`. The file must already exist; the runner does not replace a
missing identity. An explicit `CARGO_APK_DEV_KEYSTORE` environment override takes
precedence, and `CARGO_APK_DEV_KEYSTORE_PASSWORD` remains environment-only. Both
cargo-apk and Activity packaging use that identity. Without an explicit identity,
cargo-apk uses its Android user-directory default; changing that directory can
create a different signer and prevent updates to an installed app. Never uninstall
an existing app to resolve a signing mismatch when its session must be preserved.

A separate opt-in mock runs with
`python3 scripts/frontend.py run -- python3 scripts/launch-frontend-mock.py`.
It prints its isolated instance URL and public CA path. Production `homed` never loads it.

The transport gate runs native Rust executables on macOS and the task-owned API35
ARM64 emulator against isolated real TLS core routes. A separate generated Go
fixture checks future wire values. This gate does not qualify an APK UI or a
physical device. Service stubs reuse the existing Buffa messages; regenerate with
`python3 scripts/frontend.py run -- python3 scripts/generate-frontend-transport.py`.

### Configured dashboard

The normal application renders the validated dashboard tree published by the
server. Widget choice, order, responsive spans, resource bindings and visibility
come from configuration. The palette and reusable native components remain in
Slint Kit Pro and the product skin. See `docs/DESIGN.md`, `design/KIT.md` and
`docs/dashboard-native-v3.md` for the reference and configuration vocabulary.

Equipment leaves use `ProTile`; their surface opens detail, and controls emit
separate capability intentions. Detail families use `ProLightPanel`,
`ProClimatePanel`, `ProFanPanel`, `ProCoverPanel`, `ProSwitchPanel`,
`ProAlarmPanel` and `ProMediaPanel`. Decimal ranges and choice tokens come from
admitted capabilities. An absent step means continuous input, not an invented
integer grid. Exact color-temperature/range/helper editors retain the kit's
`ProDeviceControls` codecs.

`ProLayoutBoard` preserves arbitrary server forests, responsive spans and popup
routes; the fixed section structure of `ProRoomPanel` cannot replace that
contract. Resource widgets keep their typed readers. Configured custom
tap/hold/double-tap recipes retain `ProRecipeCard` or the gesture-enabled
`ProEntityTile`, rather than translating an arbitrary recipe into a tile toggle.

At capture rotation, an unchanged authorized inventory can keep its existing
pixels until the matching dashboard arrives, bounded by the old proof expiry.
Device commands, resource resolution and reader admission are suspended during
that interval. Membership, permission, document, profile or proof changes clear
the view. Media revalidates before command admission reopens; retained historical
results keep their selected range without issuing another query.
Active views keep native keyboard focus while command admission is suspended;
inactive views behind a modal leave the Tab order. Dirty text drafts may cross
only the same authorized document, capability contract and helper CAS revision,
within the old proof's expiry. They receive fresh presentation generations;
held gestures, selector sessions and pending commands are never restored. A
weak owner timer expires detached draft text even if no UI publication arrives.

The local preferences page stays open while the same profile renews its session
capture. Private data and routes still clear; logout or profile replacement
returns to Home.

Routine command completion is silent: the observed equipment value or updated
data is the feedback. Busy controls still prevent duplicate submissions; errors,
refusals and uncertain results remain visible. This does not change command
admission, acknowledgement tracking or safety confirmations.

The ignored `render_synthetic_dashboard_families` test writes product renders
without opening a profile or transport. Set `HOMEWARD_SYNTHETIC_RENDER_DIR` to an
artifact directory and run it through `scripts/frontend.py run`; it covers
phone/desktop, both themes and 100/200% text for each detail family.

`scripts/qualify-frontend-dashboard-config.py` exercises the normal executable
against isolated production SQL/auth/TLS services. Two external YAML sources
are published through the real configuration APIs while the frontend stays
open. No separate application feature or hardcoded UI fixture is needed. See
`docs/DASHBOARD-CONFIGURATION.md` for artifact arguments, native checks and the
server-only mode used for separately coordinated Android qualification.

Physical phone qualification and the matching kit revisions are recorded in
[SMARTPHONE-REVIEW.md](docs/SMARTPHONE-REVIEW.md).
