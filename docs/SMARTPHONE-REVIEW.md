# Smartphone dashboard qualification — 2026-11-02

Physical device: Samsung Galaxy S9 SM-G960F, Android 10/API 29, arm64-v8a, 360 logical pixels in portrait. The application was installed normally and updated in place with the same development signer. No uninstall, app-data reset, or physical Home Assistant commands were used. An isolated production-service composition supplied synthetic devices and lists.

## Matched sources

| Repository | Tested revision |
| --- | --- |
| Slint Kit 0.5 | `ab05e839`, `codex/homeward-0.5-disabled-keyboard` |
| Slint Kit Pro | `a4a38bc8` (after `b26f234` and `8471a79`), `codex/dashboard-responsive-polish` |
| Slint | `1.18.1` |

Release APK SHA-256: `54369e675d171639c09502a207e483164a6ec3b430e79f14230ea1a0670b2343`.

## Resulting behavior

- Server screen conditions choose mobile, tablet and desktop dashboard content. Mobile Home starts with the configured alarm and garage. The client contains no household-specific equipment catalogue or destination priorities.
- Rooms derives its directory from authorized configured room widgets and retains their exact identities and destinations.
- Inventory is available inside Settings; Back returns to Settings and retains that navigation section.
- Slint Kit reserves scrollbar gutters and dismisses dialogs on an outside click. Homeward uses the resulting visible content width.
- Compact summaries wrap their readings inside their cards. Room headers retain Back and Close at 200% text. Alarm, cover and lock actions wrap within an intrinsically sized Pro card.
- Grocery editing has one page scroll owner. Items and Common items use equal columns when there is room and stack on narrow layouts. Sections grow with their content while only visible rows are mounted, including for 10,000-item histories. Suggestions have explicit gaps and padding; trailing icons share a column. Touching an item title opens editing, with a separate checkbox and Delete action. Native Back closes the keyboard and then the editor without leaving Lists.
- Missing numeric command targets display an em dash; tile step buttons stay disabled until a target is available. Cover target position remains distinct from its observed position; the simulator deliberately permits 10–90% targets while observing 0% and 100% endpoints.
- Android font-scale configuration changes stay in the existing activity and secure-session owner. Explicit kit typography follows the resolved system font size.

## Verification

Slint Kit's official check passed. Slint Kit Pro's official check passed, including 151 UI tests, identity/gesture regressions, narrow/200% geometry, and large-list virtualization. Homeward's Clippy check, 310 application tests and 53 cache tests passed; 14 existing opt-in tests remained ignored. The configured dashboard server tests passed, along with the HA dashboard compiler, configuration activation, configuration plans and reactive-template packages.

Physical interaction receipts and screenshots are retained in the separate `smartphone-ui-review` artifact directory. The native gate passed against the APK above: simulated alarm arm/disarm and garage open/close each emitted one request per tap and reached observed confirmation; outside dismissal, Settings/Inventory Back, grocery Enter/IME/editor/picker Back, foreground resume, light/dark, portrait/landscape and 200% application text were checked. Android system font scale 1.0→1.3 changed glyph width by 1.275× without replacing the activity or process. Original OS rotation, font scale, timeout, display overrides and accessibility-service settings were restored. The phone remains connected to the isolated simulator with Lists visible for review.

## Qualification limits

The broad server suite did not pass: nine historical migration gates require retained predecessor binary manifests that were not provided to this run. Their assertions were preserved; these migration gates are outside the dashboard change. The exact failing test names are retained in `backend-suite-limit.json`.

TalkBack is not qualified. Slint 1.18.1's Android backend exposes no semantic application controls in the native UIAutomator tree; component labels alone cannot supply the missing Android accessibility bridge. OS accessibility services were not changed. Representative opaque light/dark text roles measured 4.99–17.74 contrast; this is not a full WCAG certification.

Calendar and live media playback are unavailable in the simulator. This run does not qualify a new installed Mac or tablet build.

The real alarm/PIN compatibility boundary is documented separately in [ALARM-COMPATIBILITY.md](ALARM-COMPATIBILITY.md). The current control profile and the simulated panel qualification do not establish real-panel support.
