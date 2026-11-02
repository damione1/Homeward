# Home Assistant alarm compatibility boundary

Reviewed on 2026-11-02 against the [Home Assistant integration reference](https://www.home-assistant.io/integrations/alarm_control_panel/) and its [entity contract](https://contributors.home-assistant.io/core/entities/alarm-control-panel/). No real alarm command was issued.

## Current installation, read-only observation

The main `alarm_control_panel.alarm` advertised Home, Away and Trigger, a numeric code format, and code required for arming. It did not advertise Night, Vacation or Custom bypass. `changed_by` and `next_state` were null; this read supplied no usable delay deadline. These are observations of one moment, not assumptions to compile into the client.

The Konnected panel was unavailable; its feature declaration also included Night. The GW panel advertised Away only and no code format. All three alarm entries remain unqualified in the native integration manifest. A synthetic Home/Away/Disarm test does not qualify these physical panels.

Home Assistant can supply an entity-configured default code for arm/disarm. The observed flags do not establish whether such a credential is configured. Its trigger path handles only the explicitly supplied code. The implementation must distinguish a server-resolved credential from interactive entry without exposing either secret. [HA entity contract](https://contributors.home-assistant.io/core/entities/alarm-control-panel/#codes)

## Existing foundation

Homeward already types the ten business states, including entry/exit transitions; unknown/unavailable are represented separately by reading quality. Authorization, exact resource binding, command receipts and observation-based confirmation already exist.

The current control profile deliberately admits only Home/Away/Disarm and excludes per-request codes. See `proto/home/controls/v1/controls.proto`, `contracts/controls.go`, `internal/domain/controls.go`, `client/wire/src/controls_contract.rs`, `client/app/src/control_text.rs` and `client/app/src/device_view/`.

No production alarm adapter with a backend secret reference was found. MQTT discovery explicitly excludes this domain. The existing manual-alarm work item NHA-099 describes the relevant state machine, secret-handle and injected-clock foundations; its historical status needs revalidation.

## Required extension

1. Preserve the current profile and negotiate an explicit extended control profile. Advertise operations, code policy and code format per resource; display only supported, authorized operations. Trigger belongs behind an explicit secondary action.
2. Establish an opaque, bounded credential path before accepting PINs in commands. `internal/integrations/volatile_commands.go` fingerprints the full command protobuf, and `internal/integrations/durable_commands.go` may persist those bytes. Adding a raw `code` field to the existing command would put it in durable fingerprints. Bind a secret handle to its authorized actor, resource and intention; exclude secret material from command serialization, diagnostics and replay.
3. Add a reusable `ProAlarmPanel` accepting injectable state, capabilities and typed intentions. Numeric PIN entry must preserve leading zeroes, use masked input, require explicit submission, and clear on close, resource/profile change or revocation. Its host owns credentials, transport and effects.
4. Implement and qualify the production manual/provider boundary. Keep acceptance, arming and the final armed observation distinct. Use entry/exit countdowns only when the provider supplies a deadline; otherwise show the observed transition. Expose changed-by information only when supplied.

## Acceptance checks

- Advertised operations only, including the main panel's actual feature set; unsupported or unknown contract values remain inert.
- Correct/absent/invalid codes, numeric and text formats, leading zeroes, revocation and resource changes during entry or a held gesture.
- No secret in SQL, fingerprints, logs, snapshots, preferences or retry/replay material.
- Injected-clock delay boundaries, cancellation and rearming; receipt acceptance is never treated as final effect.
- Read-only/unavailable/withdrawn observations, denied access, mismatched targets and unchanged readings never become confirmation.
- Phone 360 px, 200% text, native Back, touch targets and an explicit distinction between simulator qualification and real-panel qualification.
