# Homeward architecture

This is the agreed architectural direction, not a list of implemented subsystems.
See [engineering](engineering.md) and the README for the actual foundation boundaries.

## Server

One modular Go process owns the state store, registries, actions, event distribution,
automation/script runtime, scheduling, configuration and administration. Normal home operation
must remain independent of the graphical client, AI agents and cloud connectivity wherever
the underlying devices support local operation.

Current state and operational event distribution live in memory. PostgreSQL is the preferred
durable store; a bounded asynchronous Recorder must not block automation execution.
YAML bundles describe declarative configuration. No Redis, Kafka or Kubernetes dependency is
required for the initial deployment.

## Integration boundary

Prefer embedded Go integrations with direct calls to domain ports. Keep an optional,
versioned Protobuf/gRPC projection of the same functional Host contract for integrations
running in separate processes, including Python, Rust and Node.js implementations.
Each external process needs explicit lifecycle, permissions, health, bounded queues and
reconnection behavior. It accesses server functionality through the Host rather than shared SQL.
The runtime and launcher still need implementation.

Integrations describe devices and capabilities, publish observations/events, receive commands
and report outcomes. Network acknowledgement and physical confirmation are distinct.
Do not replay uncertain physical actions automatically after a crash or reconnect.

Home Assistant provider libraries can be reused through adapters when justified. A general
compatibility wrapper for executing Home Assistant integration components is **deferred**.
It is not a dependency of the server or the first integration milestone.

## Contracts and compatibility

Protobuf and Buf define and check the public transport contracts. Connect-Go is the planned
server transport, with standard gRPC interoperability for external integrations.
Internal Go domain models remain independent of generated wire types.

Home Assistant YAML compatibility is explicit and versioned, initially targeting Core 2026.10.1.
Parse into a strict internal representation and prove semantics with deterministic fixtures
before claiming compatibility. Existing synthetic fixture checks are not a full HA-versus-Go
runtime comparison. A temporary HA bridge can support incremental migration, with exactly
one active command owner for each transferred physical capability.

## Scope and clients

The existing scope manifest freezes the initial 850 selected IDs, including 19 open diagnostics;
89 automation/script/scene objects remain inactive pending individual decisions.
Ecobee control is an explicitly requested amendment: qualify a targeted local HomeKit or
dedicated Ecobee route and version the scope change without rewriting the baseline.

The native client will use Rust/Slint and [damione1/slint-kit](https://github.com/damione1/slint-kit),
with Android and macOS first. Client work is deferred and does not block the server MVP.

## Data retention and recovery targets

The agreed targets are 7 days for useful state history, 7 days for execution traces,
13 months for hourly aggregates and 90 days for administrative audit.
Configuration, lists and calendars target an RPO of at most one hour; server recovery targets
an RTO of at most 30 minutes after intervention. These are future implementation and measurement
requirements, not operational backup or restoration guarantees. Backup retention remains open.
Historical MariaDB import is deferred; the previous history can remain readable during transition.
