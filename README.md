# Homeward

A local-first home automation platform built around a modular Go server.

Homeward aims to provide dependable home control, Home Assistant configuration compatibility,
and an open integration ecosystem. Embedded Go integrations are preferred, with optional
external integrations through a versioned Protobuf/gRPC contract.

The project is at an early foundation stage. Operational device drivers, automation runtime,
transport services and the native client are still to be implemented. The server is the current
focus; the Rust/Slint client and Matter are deferred.

Development documentation is maintained in the project's Obsidian vault.

## License

[GNU General Public License v3.0](LICENSE). Third-party dependencies retain their respective licenses.
