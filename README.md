# Homeward

A local-first home automation platform, built around a modular Go server.

Homeward aims to provide dependable home control, Home Assistant configuration compatibility,
and an open integration ecosystem. Embedded Go integrations are preferred; optional external
integrations will communicate through a versioned Protobuf/gRPC contract in the language
best suited to their protocol libraries.

## Current status

Early foundations are implemented: strict bootstrap configuration and lifecycle, a frozen
migration-scope manifest, a source-linked Home Assistant compatibility catalogue with synthetic
fixtures, and Protobuf contracts with shared Go/Rust codec tests. A preparatory Matter study
is included; Matter implementation is deferred.

No operational device driver, automation engine, Connect/gRPC server, physical command path,
production migration, or Slint UI is delivered yet. The current listener is a lifecycle probe.
The native Rust/Slint client is deferred while the server is developed.

## Run and check

Use the pinned toolchain described in [development boundaries](docs/engineering.md).

```sh
go run ./cmd/homed -config config/example.yaml
```

Send SIGTERM to stop. Run the complete local checks from the repository root:

```sh
make check
```

## Direction

The initial integration priorities are Zigbee2MQTT/MQTT and Lutron for lighting and outlets,
Ecobee and Sinopé for heating, then Hilo for consumption. Matter comes last.
See the [architecture](docs/architecture.md) and [roadmap](docs/roadmap.md).
The [strict-types review](docs/strict-types-review.md) documents how the existing foundations
align with the project's design philosophy and which boundaries still need strengthening.

The canonical repository is [damione1/Homeward](https://github.com/damione1/Homeward),
with development on `master`.

## License

[GNU General Public License v3.0](LICENSE). Third-party dependencies retain their respective licenses.
