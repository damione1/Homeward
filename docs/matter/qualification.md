# Matter / Thread qualification — NHA-012, G7

Documentary investigation completed on 2026-10-17, within the two-day budget.
Outcome: retain a separate maintained OHF Matter.js controller as the direction;
production readiness and the household Thread path remain conditional. No commissioning,
network change, production query, device command or HomeKit conversion was performed.

## Separate roles and present evidence

| Role | Owns | Household evidence | Conclusion |
|---|---|---|---|
| Apple TV Thread Border Router (TBR) | IP routing between Thread mesh and LAN | Inventory says Apple TV 4K gen 3; exact variant unknown | Not qualified as TBR |
| Matter controller | fabric credentials, node commissioning, subscriptions, persistent storage | Existing app labelled Matter Server 9.2.0, image/runtime unverified; zero nodes in documentary diagnostics | No node available by inference; no storage migration authorized |
| Matter-Hub bridge | exposes selected HA entities outward as Matter endpoints | App 3.0.4 recorded running | Distinct from controller, no new bridge requirement |
| Product Go adapter | projects typed node/endpoint/cluster data through versioned API | Not implemented | Future slice, no direct CHIP/cgo dependency |

Local sources: vault `Protocoles et passerelles.md` (Matter/Thread),
`Architecture/Matrice des intégrations.md` (Matter/Thread),
`Architecture/Sources et versions architecture.md`, `Appareils.md` (apple_tv).
These documents are observations, not fresh network measurements.
No fabric identifier, Thread dataset, credential or commissioning code is exported.
The retained Apple TV media integration remains distinct from Thread and HomeKit.

## Versioned primary references

1. [OHF Matter.js Server README, fbbb20c986f9be194e55f5f56e8d3546e980899f](https://github.com/matter-js/matterjs-server/blob/fbbb20c986f9be194e55f5f56e8d3546e980899f/README.md)
   and [version.txt](https://github.com/matter-js/matterjs-server/blob/fbbb20c986f9be194e55f5f56e8d3546e980899f/version.txt):
   maintained controller direction, JavaScript Matter SDK and WebSocket API. At this reference
   the README still identifies beta/re-certification pending; version.txt is
   `1.5.0-alpha.0-20261017-077942f`. An alpha string, app version and protocol version
   are different identifiers. Neither this string nor the README certifies the household image.
2. Maintenance checked 2026-10-17 through GitHub repository/commit APIs: not archived,
   pushed_at `2026-10-17T19:05:43Z`, HEAD
   `d84524fc0b46aac6cda1d651543fa0814ef803ad`, commit date `2026-10-17T19:05:41Z`.
   [README at that HEAD](https://github.com/matter-js/matterjs-server/blob/d84524fc0b46aac6cda1d651543fa0814ef803ad/README.md)
   was also read and still marks beta/re-certification pending. It additionally warns that
   its current Thread commissioning path needs a network name not provided by the described
   setup; BLE/mobile credential handoff is therefore an explicit unresolved trial condition.
   This establishes ongoing activity, not stable release/production suitability.
   Re-check release, image digest, storage compatibility and certification at implementation time.
3. [Old Python server README, 4c820ed12bac349dda372031e9757c67b9fd9048](https://github.com/matter-js/python-matter-server/blob/4c820ed12bac349dda372031e9757c67b9fd9048/README.md):
   8.1.2 is the final Python release and upstream directs migration to Matter.js.
   Do not choose it as the new long-term runtime or equate it with app label 9.2.0.
4. [Apple support 102078](https://support.apple.com/en-us/102078), published 2026-01-13,
   read 2026-10-17: third-generation Apple TV 4K **Wi-Fi + Ethernet** is listed for Thread;
   the inventory does not establish that variant. Wi-Fi-only third-generation must not be
   assumed eligible. Apple TV being a home hub does not expose this product's controller API.
5. [HA Thread documentation](https://www.home-assistant.io/integrations/thread/), read 2026-10-17:
   network/credentials and border routing are separate from a Matter controller. IPv6 and
   multicast discovery must be demonstrated on the intended path, not guessed from an IP address.
6. [Matter-Hub README, bf6af34dcfed729bce1ec50588dccf8f7a7c0b4f](https://github.com/t0bst4r/home-assistant-matter-hub/blob/bf6af34dcfed729bce1ec50588dccf8f7a7c0b4f/README.md):
   a bridge publishes HA entities into Matter. It neither proves commissioned native nodes nor
   replaces an independent controller. Its presence does not convert an excluded HomeKit device.

## Proofs required before opening G7

| Condition | Evidence to obtain later, in an authorized isolated trial | Current status |
|---|---|---|
| Exact Apple TV variant | Model number/variant, firmware, physical/network route; compare to Apple eligibility | Missing, TBR unqualified |
| LAN/Thread reachability | IPv6 unicast routes, mDNS/multicast discovery across intended interfaces/VLANs, firewall/MTU; record route loss/recovery | Not measured |
| Current server identity | Image repository plus immutable digest, runtime version, architecture, storage layout and API/descriptor version; app label insufficient | Unknown |
| Controller ownership | Separate test fabric/controller namespace, encrypted secret storage/backup, no sharing live writable storage | Not created |
| Commissioning path | Real compatible test accessory; authorize opening its window; distinguish new fabric vs multi-admin; BLE/mobile credential handoff if needed | Not authorized or executed |
| End-to-end state | Snapshot, subscription, read and harmless confirmed test command correlated to fresh report | Not executed |
| Recovery | Lose TBR, restore route; restart controller; restore isolated encrypted fabric backup without duplicate node or unsafe replay | Not executed |

Matter can use Wi-Fi/Ethernet without Thread; a Thread TBR gate only applies to Thread accessories.
An unknown Apple TV variant blocks the proposed Apple TV Thread path, not all Matter research.
A positive model match alone proves eligibility, not routing or successful commissioning.
Zero observed nodes remains zero; a blank or unavailable diagnostic is not positive discovery.

## Trial protocol for later authorization

1. Freeze all evidence above, select a supported synthetic/test accessory and a rollback owner.
   Preserve current production authority. No fabric/store copy is attempted until runtime/storage
   compatibility is proven. Use a separate test storage directory and immutable server image.
2. Confirm eligible TBR model and actual IPv6/mDNS path. Record sanitized pass/fail and version
   metadata, not network datasets or credentials. If the model remains unknown, stop the Thread
   route qualification; keep status conditional and do not commission.
3. Start the chosen controller in isolation and verify its versioned WebSocket contract/auth boundary.
   Record safe snapshot/subscription before any requested command. Go does not own the fabric.
4. With explicit later authorization, commission only the test accessory. Multi-admin adds an
   actually supported Matter accessory to another fabric; it is not HomeKit-to-Matter conversion.
   Record one node only after discovery, identity and descriptor evidence. Confirm fresh state.
5. Request a safe absolute test action, verify protocol receipt separately from physical confirmation.
   Simulate route loss and controller restart; never replay an uncertain physical command blindly.
6. Restore an isolated backup and verify identity/subscriptions remain consistent; shut down test
   controller/remove test fabric under its documented process. Leave production credentials untouched.

Pass requires all required evidence with versions and observed results. Missing evidence yields
`conditional`, with named blockers. Failed recovery/storage compatibility yields stop/re-design.
The documentary investigation can close with these conditions open; implementation G7 cannot.

## Negative scenario and reproducible check

`assessment.json` contains the documentary unknown-variant, zero-node input.
`python3 scripts/check-matter.py` checks the outcome is unqualified, zero available nodes and
explicit missing model/network/image/commissioning/Thread-route/fabric/end-to-end/recovery gates. It also tests known eligible hardware
without network proof, each individually missing operational proof and a hypothetical all-proven isolated scenario.
The evidence predicate qualifies the proposed Apple TV Thread path; Wi-Fi/Ethernet-only Matter needs
a separate route-specific assessment and does not require a Thread TBR. These are pure evidence-rule
tests, not measurements or commissioning. The standard `make check` runs this validation.
