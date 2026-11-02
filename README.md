# Homeward

A local-first home automation platform built around a modular Go server.

Homeward aims to provide dependable home control, Home Assistant configuration compatibility,
and an open integration ecosystem. Embedded Go integrations are preferred, with optional
external integrations through a versioned Protobuf/gRPC contract.

The server and native Rust/Slint client are under active development.

Development documentation is maintained in the project's Obsidian vault.

## License

[GNU General Public License v3.0](LICENSE). Third-party dependencies retain their respective licenses.

## Native development evaluation

The frontend consumes Slint Kit 0.5.1 and a local Slint Kit Pro checkout. Configure
both matching kit checkouts before Cargo loads the workspace (absolute paths may
point anywhere). Keep the free kit on the `v0.5.1` revision, or on a branch that
contains the local corrections being evaluated; its runtime and build crates must
resolve to the same version and source tree.

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

`scripts/qualify-frontend-dashboard-config.py` exercises the normal executable
against isolated production SQL/auth/TLS services. Two external YAML sources
are published through the real configuration APIs while the frontend stays
open. No separate application feature or hardcoded UI fixture is needed. See
`docs/DASHBOARD-CONFIGURATION.md` for artifact arguments, native checks and the
server-only mode used for separately coordinated Android qualification.

Physical phone qualification and the matching kit revisions are recorded in
[SMARTPHONE-REVIEW.md](docs/SMARTPHONE-REVIEW.md).
