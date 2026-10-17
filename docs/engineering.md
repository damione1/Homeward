# Development boundaries

Go 1.25.2 is pinned in go.mod; Rust 1.98.1 and components in rust-toolchain.toml;
Buf 1.52.1 and protoc 29.3 are the schema toolchain. Python 3.12.8 runs standard-library-only validation scripts.
Install Go/Rust/Buf/protoc at these versions, then `go mod download` and `cargo check --workspace`.
All checks run from repository root using `make check`.

`cmd/homed` is the composition root. `internal/domain` owns only domain identities;
future modules depend on domain ports and never on concrete drivers, Rust, or generated wire types.
The import check reads Go's actual transitive dependency graph.
`client/model` is the native client boundary. UI and Connect transport qualification follow in their own tickets.
`proto/` owns the wire contract; it is independent of domain and drivers.

The listener is a lifecycle probe: it closes connections immediately, serves no HTTP and has no operational command API.
Configuration is parsed strictly (known fields, one document, numeric listen address) before binding.
SIGTERM cancels context, closes the listener, joins tasks and exits normally.
No production connections, discovery or device commands are made.

No project license has been selected; do not infer one from dependencies. Keep this repository private.
