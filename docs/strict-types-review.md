# Foundation review against the strict-types philosophy

Reviewed on 2026-10-17 against [Damien's article](https://damiengoehrig.ca/blog/closer-to-the-machine-strict-types/).
Scope: the five delivered foundation tickets imported from provisional commit `9d18095`,
plus the Homeward module rename. Generated codecs were checked for reproducibility;
handwritten Go/Rust code, schemas, boundary checks and tests were read directly.

## Verdict

**Partially aligned.** The wire-contract foundation supports the intended approach, but the
current domain types and boundary examples do not yet enforce it end to end. A passing
`make check` is evidence of the checks that exist, not a general proof that invalid business
states are unrepresentable.

The relevant principles are: model closed sets explicitly, distinguish open identifiers,
parse untrusted input into constrained values, derive cross-language contracts from one
source, and make missing handling visible during development. The findings below are
our code assessment, not a claim that the article audited this repository.

## Findings

### 1. Constrained domain identities are not implemented yet

[Go EntityID](../internal/domain/entity.go#L4) is an exported string-backed type, and
[Rust EntityId](../client/model/src/lib.rs#L3) has a public String field. They distinguish
names at the type level, but allow empty or otherwise invalid values to be constructed.
Neither file has a parser or a constructor enforcing an identity invariant.

This is a foundation placeholder, not completion of NHA-013. Before registries, drivers
and action admission consume these identities, define their invariants and controlled
construction. Use distinct types for open identifier categories rather than a global enum
of device names. In Go, explicitly account for zero values: unexported representation and
constructors alone cannot eliminate every invalid zero value.

### 2. The Rust boundary example duplicates a closed error vocabulary as strings

[validate_command](../client/wire/src/lib.rs#L11) returns `Result<(), &'static str>`,
with literals such as `stale_epoch` and `invalid_arguments`. The wire schema already defines
[ErrorCode and ContractError](../proto/home/actions/v1/actions.proto#L39), and the Go
example returns those structured errors. Rust currently has a second vocabulary without
compile-time continuity to the schema, including a different granularity for invalid arguments.

Before this example becomes an operational boundary, return the generated structured error,
or a domain error with an explicit exhaustive conversion. Retain detail through typed codes
and field paths instead of adding parallel string spellings. Client work remains deferred;
this finding does not start a Rust client implementation.

### 3. Configuration is checked but remains a freely mutable string

[Config](../internal/config/config.go#L14) contains exported `Listen string`. Parse rejects
bad YAML, unknown fields, extra documents, hostnames and invalid numeric ports. The server
then [validates again](../internal/server/server.go#L13), because callers can construct or
mutate Config directly without using Parse.

The operational bootstrap protects its listener boundary, but this is validation of a wide
representation rather than a constrained value carried onward. A future configuration
model should parse its listen endpoint into a suitable typed representation, with controlled
construction and deliberate zero-value behavior.

### 4. Wire compatibility must not become permissive domain admission

The schemas use enums, oneof and optional fields, but decoded Protobuf still admits absent
branches, unknown enum numbers and unsupported extensions. Preserving unknown values for
round trips is useful at the wire boundary; treating them as valid operational states is a
separate decision. The examples reject missing targets and unsupported command schemas,
but there is not yet a complete wire-to-domain parser for states, actions or quality.

`ValidateCommand` explicitly omits authorization, expiry and identity resolution. These are
future admission work, not properties proven by the codec tests. Neither language should
pass a raw decoded command to a future physical dispatcher based only on this example.

For future closed domain enums, make exhaustiveness a checked rule. Rust supports exhaustive
matches; the workspace does not currently enforce the article's wildcard-match lint on
handwritten domain code. Go switches are not automatically exhaustive; choose and test an
appropriate analysis rule where this guarantee is required. Do not apply that rule blindly
to generated wire code handling unknown protocol values.

## What already aligns

- Protobuf definitions are the wire source of truth; Go/Rust bindings are generated and their
  exact reproduction is checked. The rename changes only Go import-path metadata in schemas
  and their explicitly reviewed baseline, not field numbers or golden message bytes.
- Closed protocol sets use enums; alternatives use oneof; optional revision/source sequence
  preserve the distinction between missing and zero. Availability, freshness and provenance
  are named dimensions instead of incidental nulls or ambiguous booleans.
- Twelve binary goldens exercise both codecs, including unknown values/fields, uint64 above
  JavaScript's safe integer range, explicit zero presence and decimal-string precision.
  These prove representation behavior, not semantic validity of every payload.
- Buf checks compatibility, and a negative probe proves that deleting an unreserved field
  is rejected. Generated-code checks catch manual drift.
- The Go dependency boundary keeps domain code independent of generated transport, concrete
  server/config and future driver layers. This is checked against the actual import graph.
- Bootstrap cancellation and SIGTERM behavior are tested without real device commands.
  No arbitrary-map business model has been introduced. The `any` local in the YAML parser
  only detects an extra YAML document; it does not circulate through the domain.

## Evidence and next steps

`make check` passed locally on the renamed module: Go vet/race tests, Rust formatting/Clippy/
tests/check, Buf lint/breaking, bootstrap process test, scope proofs, synthetic fixture oracles,
binary goldens, negative compatibility probe, generated-code reproduction and Matter study checks.
Live-vault set equality and original source hashes also passed for the frozen 850-ID baseline.
The ten fixture oracles explicitly report that the HA reference runtime was not executed.

Prioritize constrained Go domain identities in NHA-013 and the future wire-to-domain admission
boundary before introducing real drivers. Track typed Rust error conversion and exhaustive
domain handling when that layer is resumed. Define database enum/code generation with the
PostgreSQL work; no database schema exists yet to audit. No performance benchmark, operational
gRPC service, HA compatibility runtime or hardware qualification is claimed by this review.
