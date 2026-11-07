# Native alarm capability boundary

The native client renders the alarm capability supplied by the server. Home, Away, Night and Disarm are explicit operations; observing an armed state does not imply that its corresponding operation is supported. Unsupported operations and unavailable observations stay inert. Trigger and additional arming modes are outside this command contract.

The server owns device credentials. A provider requiring a disarm code receives it through an explicitly configured server secret; the client command contains only the admitted operation. The provider must withhold an operation whose required credential is unavailable. The native panel therefore omits its keypad for this credential policy; it does not pretend that a device requiring a code accepts a blank one.

Interactive PIN entry is a separate capability. It must not add a raw PIN to `SetAlarmCommand`: command serialization, durable receipts and fingerprints are not a secret transport. Any future interactive flow requires a short-lived, actor/resource/operation-bound secret exchange before the UI can offer the keypad.

`ProAlarmPanel` receives observed condition, state text and supported choices. Its modes and icons use the same opaque choice tokens. The host retains authorization, exact capability binding, command receipts and observation-based confirmation. A selected mode represents an observation; clicking another mode does not optimistically change it. Existing safety confirmation applies before submission.

Qualification uses synthetic capabilities for unknown values, unsupported operations, unavailable readings, revoked access, stale gestures and command suspension. Device qualification is separate: record the freshly observed state before an authorized trial, issue each operation once, then restore and verify the original state. Acceptance, protocol acknowledgement, arming transitions and final observed state remain distinct.
