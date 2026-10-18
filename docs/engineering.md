# Development boundaries

Go 1.25.2 is pinned in go.mod; Rust 1.98.1 and components in rust-toolchain.toml;
Buf 1.52.1 and protoc 29.3 are the schema toolchain. Python 3.12.8 runs standard-library-only validation scripts.
Install Go/Rust/Buf/protoc at these versions, then `go mod download` and `cargo check --workspace`.
All checks run from repository root using `make check`.

Follow [Damien's strict-types philosophy](https://damiengoehrig.ca/blog/closer-to-the-machine-strict-types/)
when implementing domain and boundary code. The [foundation review](strict-types-review.md)
records the existing gaps; generated schemas alone do not establish domain validity.
Keep closed vocabularies typed, open identifiers distinct, parsing at boundaries and generated
contracts reproducible. Do not treat wire compatibility or a passing test suite as proof of
complete domain admission or physical command safety.

`cmd/homed` is the composition root. `internal/domain` owns only domain identities;
future modules depend on domain ports and never on concrete drivers, Rust, or generated wire types.
The import check reads Go's actual transitive dependency graph.
`client/model` is the native client boundary. UI and Connect transport qualification follow in their own tickets.
`proto/` owns the wire contract; it is independent of domain and drivers.

The listener is a lifecycle probe: it closes connections immediately, serves no HTTP and has no operational command API.
Configuration is parsed strictly (known fields, one document, numeric listen address) before binding.
SIGTERM cancels context, closes the listener, joins tasks and exits normally.
No production connections, discovery or device commands are made.

Homeward is published in `github.com/damione1/Homeward` under the GPLv3 license already
selected in the repository. See `LICENSE`; dependencies retain their own licenses.
The canonical branch is `master`. The earlier `new-home-assistant` repository is a historical
source of the foundation commits, not the current development destination.

Embedded Go integrations are preferred. Optional external integrations use a versioned
Protobuf/gRPC Host contract and may use another compatible language. The Host runtime and
external launcher are planned work, not delivered by this bootstrap.

The Homeward rename explicitly updates only `go_package` import-path metadata in the
Protobuf sources and the frozen baseline from the provisional module to the canonical module.
This is a reviewed source-namespace migration; package names, services, fields, field numbers
and golden message bytes remain unchanged. Future baseline changes still require explicit
contract review and must not conceal breaking changes.
