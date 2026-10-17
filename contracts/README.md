# Public wire contract v1 — NHA-004

Protobuf-only foundation. No HTTP server, client networking, Connect/tonic selection, driver
catalogue or physical dispatcher is implemented. Go generated protobuf runtime/generator 1.36.11;
Rust buffa/buffa-types/protoc-gen-buffa 0.9.2; Buf 1.52.1 and protoc 29.3. Cargo/Go locks are checked in.
The client codec stays in `client/wire`, outside the domain model. A future Connect spike can use
these buffa messages without assuming transport/device conformance.

`home.registry.v1`: internal-or-HA EntityRef, descriptions with versioned schema ID and descriptor
hash. Unknown descriptions can be displayed; raw extension bytes never authorize execution.
`home.actions.v1`: explicit epoch/config revision, optional expected revision, typed switch arguments
or versioned extension envelope, expiration timestamp, endpoint and structured errors/action statuses.
Accepted/acknowledged/confirmed are distinct; no exactly-once or automatic physical retry claim.
Authenticated actor/authorization and expiry enforcement belong to the future admission service;
the pure examples validate only identity, epoch/config and supported argument shape.
`home.state.v1`: independent availability/freshness/provenance dimensions, optional source sequence,
timestamps, decimal string precision, paged snapshot and filtered resumable stream.

Both Go and Rust admission examples reject unknown schemas/extensions before execution and reject
missing or empty EntityRef branches. They are pure validation examples; there is no dispatcher.
An extension codec must later be registered against schema ID/major/hash and authorized before its
payload can be validated/executed. An unknown enum is retained numerically and must not be treated
as an affirmative availability/permission decision.

## Coherent exchange

1. `OpenView` fixes epoch E, accepted global sequence S=100, config C=9007199254740993,
   authorization A=4 and filter hash F. `snapshot-page-1` returns snapshot-a and page-2 token.
2. `ReadPage(snapshot-a,page-2)` returns the same E/S/C/A/F, same expiration and no next token.
   A page token is opaque and server-bound to its snapshot, identity and expiry; never merge generations.
3. `WatchView` resumes at S+1. `filtered-stream` has contiguous stream sequence 1 but global sequence
   108: filtering can omit global events, so a global gap is not loss. Its cursor retains E/C/A/F.
4. `resync-required` is terminal stream sequence 2, configuration_changed. Stop applying old
   frames/commands, refresh snapshot, then atomically replace the generation. Epoch/filter/rights
   changes or expired replay require the corresponding resync reason, never partial silent replay.
5. Rights revocation must purge forbidden cached data immediately; auth checks override snapshot
   convenience. Snapshot TTL and bounded replay capacity are future service policies, not tested here.

Examples cover states and capability descriptions. Full registry metadata/dashboard payloads and
transport implementation remain separate tickets. Snapshot token binding, paging and replay semantics
are documented contracts; no running service is claimed by these golden examples.

## Compatibility and proof

Run `make check` from repository root. It includes `buf lint`,
`buf breaking --against .compat/baseline`, both language tests, reproduction of twelve
textproto→binary golden messages, generated-code freshness and an actual negative Buf probe.
The negative probe copies schemas to an external temporary directory, deletes target field #4
without reservation, runs breaking against the frozen baseline, requires rejection, then cleans up.

Never reuse field numbers. Removed names/numbers are reserved (existing examples in EntityRef and
CommandRequest). Enums retain UNSPECIFIED=0 and unknown numeric values. Additive fields remain
unknown-but-preserved by the codecs. Baseline is frozen under `.compat/baseline`; update it only
as an explicit API review, never to silence breaking checks.

Both languages roundtrip the same bytes: command uint64 above 2^53, expected_revision present zero,
nanosecond Timestamp, unknown availability=99, uint64 max, decimal string, source_seq present zero,
unknown extension and readable description, two same-generation snapshot pages, filtered frame,
terminal resync and structured error. Tests append an unknown field #99 and preserve it.
Three negative goldens cover absent target and empty internal-ID/HA-alias branches with matching
rejection in Go and Rust. Rust's four integration tests and Go's four contract tests exercise these.
`sh scripts/generate-proto.sh` regenerates committed bindings with pinned tools;
`sh scripts/check-generated.sh` generates to a cleaned external temp directory and compares bytes.
No production endpoint is contacted.
