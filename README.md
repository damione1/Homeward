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

The frontend currently consumes a local Slint Kit Pro checkout. Configure both
kit checkouts before Cargo loads the workspace (absolute paths may point anywhere):

```sh
python3 scripts/frontend.py configure --free-kit /path/to/slint-kit --pro-kit /path/to/slint-kit-pro --backend-cache /path/to/shared/homeward-cache
python3 scripts/frontend.py run -- brew install --cask android-commandlinetools
python3 scripts/frontend.py run --reserve-gb 14 -- python3 scripts/setup-frontend-android.py
python3 scripts/frontend.py run -- cargo install cargo-apk --version 0.10.0 --locked --root .tools
make frontend-desktop
make frontend-android
# Export the frozen NHA-036/NHA-055 migration artifacts documented in Obsidian first.
python3 scripts/frontend.py run -- make check
```

Configuration and kit links stay local. The runner reuses `.tools` and the shared Cargo
output, reserves the existing backend build locks, records source manifests and checks
the frontend's separate 40 GB storage budget before, during and after each round.
It retains at least 50 GB free under the current shared-host policy. The Android SDK and existing Java installation are configured
for this Homebrew development host; see the Obsidian ticket for the evaluated versions.

A separate opt-in mock runs with
`python3 scripts/frontend.py run -- python3 scripts/launch-frontend-mock.py`.
It prints its isolated instance URL and public CA path. Production `homed` never loads it.
