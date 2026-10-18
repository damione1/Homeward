# Homeward roadmap

The server is the current focus. This roadmap separates delivered foundations from planned
capabilities; it does not activate devices or change an existing Home Assistant installation.

## Delivered foundations

- NHA-001: Go bootstrap, strict configuration, lifecycle checks and Rust domain boundary.
- NHA-002: frozen initial 850-ID scope, independent source proofs and 19 diagnostic markers.
- NHA-003: HA construct catalogue and ten synthetic fixture/oracle scenarios, without a Go
  automation runtime or claimed differential compatibility.
- NHA-004: Protobuf messages/services, generated Go/Rust codecs and shared binary goldens.
- NHA-012: documentary Matter/Thread assessment, with physical qualification still outstanding.

These commits originate in the provisional project repository; their history is preserved.
Homeward starts with the GPLv3 license selected in its initial repository commit.

## Delivery order

1. Complete the server domain, Host, PostgreSQL, lifecycle, state/action APIs and minimum
   configuration/automation semantics needed by the first vertical slice.
2. Deliver lighting and outlets through MQTT/Zigbee2MQTT, then Lutron Caséta LEAP.
   Zigbee2MQTT already runs separately; Mosquitto externalization is a distinct migration task.
   Prove the vertical slice with an authenticated Go API/CLI harness; no graphical client gate.
3. Deliver Ecobee and Sinopé/Neviweb heating control, with explicit activity/freshness and
   estimated HVAC consumption. Qualify the Ecobee transport and version its scope amendment.
4. Deliver Hilo consumption observations, then the retained tariffs, challenges and rewards.
   Reconcile physical identities before merging bindings or assigning command ownership;
   retain source provenance and avoid counting the same energy twice.
5. Expand the remaining useful integrations and revisit the native client for Android/macOS.
   Implement and qualify Matter last; disconnected switches do not constitute a commissioned
   installation.
6. Complete staged migration and isolated restore qualification before claiming independence
   from Home Assistant. Historical import remains optional and deferred.

## Deferred exploration

Executing HA integration components through a generic compatibility wrapper is set aside.
Optional external integrations remain part of the architecture, but a wrapper is neither
required nor adopted. No automatic catalogue download, arbitrary integration loading or
production rollout is delivered by these foundations.
