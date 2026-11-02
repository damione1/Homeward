# Homeward : Lovelace layout, native compact controls

This is the implementation specification for Damien’s UX refinement. The HTML supplies the visual language; his Home Assistant screenshots and exported configuration supply the organisation and control conventions. This is an adaptation, not a pixel clone of HA’s theme.

## Evidence and decisions

The reference exports were read without modifying Home Assistant. `Homeward-artifacts/ux-lovelace-reference/dashboard-tablet.json` is the exact main reference: Quick Controls, nine room buttons, Calendar, and a fourth section containing room/global popups. `dashboard.json` is the general `lovelace` dashboard with 18 views, including Garage, Scenes, At a glance, Lists and irrigation. These exports inform a server-owned source, never a compiled client installation catalogue.

The main supplied HA image is 2678 × 1760 pixels. Its room pills occupy y208–320, 336–448, …, 1232–1344: 112 pixels high with 16-pixel gaps. Normalising to a 1280-pixel board gives approximately 54 and 8; the native target is 56 and 8 logical pixels. This compares proportions and does not assume a screenshot device-pixel ratio. The measured HTML palette, typography and geometry remain in [KIT.md](../design/KIT.md).

The old 120-high room mosaic is superseded by compact room rows. Simple observations and switches must not occupy the area of a thermostat or media detail. A grid supplies width and reading order; it never stretches a leaf vertically merely to fill another leaf’s height.

## Design plan

- **Archetype:** one native dashboard window, persistent app navigation, a server-composed board and one detail surface. A room is a useful compact control panel.
- **Information architecture:** Dashboard → Room → Equipment detail. Sections and destinations remain server-owned. Equipment detail replaces the same surface’s body instead of adding another dimmed modal.
- **Command map:** pointer/touch activates an explicit control; Tab/Shift-Tab moves focus; Return/Space activates a button; arrows adjust a focused slider/choice. Escape and Android Back return one level. Existing shortcuts retain their meanings. Return restores the source’s focus and scroll position. A row body and trailing control have distinct, nonoverlapping hit areas.
- **Empty states:** missing, unavailable, unauthorized, stale, loading and successfully empty differ. Show concise local status. Never fabricate temperatures, forecasts, artwork, events, entities or list items.
- **Feeling:** quick, calm and confident. Preserve the tablet’s existing responsiveness. Content stays stable during interaction. Submitting a command never becomes a fabricated observed device state.

## Main dashboard

At normal tablet/desktop widths, use three balanced, source-defined columns with a 20-pixel gap and 20–24-pixel outer margin:

| Column | Content | Treatment |
| --- | --- | --- |
| Quick Controls | Clock/weather, home thermostat, small scene/routine actions, fan | Dense cards and compact inline controls |
| Rooms | Living Room, Kitchen, Dining Room, Master Bedroom, Eugénie’s, Bathroom, Office, Guest Bedroom, Outdoor | One vertical list of nine 56-pixel rows |
| Agenda and lists | Configured calendar preview and optional shopping/list preview | Bounded previews and an explicit configured route to full content |

Order, labels and membership belong to the example server document. Another installation can reorder, rename, replace or omit everything without rebuilding. Room state uses declared exact sources, never the first light or climate found in a room.

Keep the reference status facts (alarm, weather, garage, leaks, package and washer) in a compact chip strip when configured. The example declares the two leak sources separately; it does not invent an aggregate observation. Neutral facts remain quiet; exceptional conditions have icons and words as well as colour. Do not repeat each fact as a giant status card below.

Music/Security/Climate/More are contextual dashboard actions. They may form a compact two-by-two shortcut group in Quick Controls; the existing rail/bottom bar handles app navigation. Do not add a second floating navigation layer over the app’s bottom bar. Frequent room, climate and media actions must not be discoverable only through More.

Use a source minimum column width near 320. Reflow to two columns and then one as width/text require; retain source reading order, with rooms before secondary agenda/list information. No renderer sort by domain or hardcoded room priority. In compact configurations, a source-provided “Pièces” shortcut near the first visible controls keeps rooms accessible without searching a long board. Empty sections retain no large blank frames.

Clock/weather uses about 144 pixels for the summary. Real forecast data may justify a 240–280-pixel card. Missing forecasts consume no placeholder space. The current time, weather and date remain readable without opening another surface.

## Sizing and visual grammar

These are intrinsic base targets, not fixed heights that clip translated/enlarged text. Components grow to fit actual labels and values.

| Presentation | Base logical height | Content |
| --- | --- | --- |
| Status chip | 36 | Icon and one short fact |
| Room row | 56 | 36–40 icon well, name, state, optional exact temperature/disclosure |
| Simple entity/observation | 56–64 | Name, one state/value, optional single trailing action |
| Scene/routine shortcut | 56–80 | Label, recognisable icon; last run secondary |
| Numeric stepper / simple switch | 64–80 | Observed value and explicit compact control |
| Light/fan dimmer | 96 | Header and one slider or discrete speed row |
| Compact thermostat | 112 | Name, current/mode, separate setpoint and −/+ |
| Compact media player | 128 | Small artwork/identity, metadata, supported transport/volume |
| Room header | 56 | Identity and close/back, fixed while body scrolls |

Keep HTML’s neutral canvas/surfaces, violet navigation accent and semantic lighting/heating/air/security tints. Use 16-pixel compact tile radii and 20–22-pixel standalone card radii. Related rows need only a quiet border/separator, not a large shadow per entity. Active colour belongs mainly in an icon well, status, slider fill or selected segment. Broad tint is reserved for a meaningful selected/active group.

Names use 14/20, states 12/16 and section headings 15/22. Compact thermostat values are about 22–24, not a 40-pixel hero value. Detailed setpoints may use 48–56. Values use tabular digits; truncated names remain complete to accessibility and hover help.

Mac visuals may be 28–32 pixels while touch hit areas are at least 44–48, separated from neighbours. At 200% text, derive height from text lines plus padding: a room row is approximately 88 rather than blindly doubling every margin/radius. Reflow before clipping. Enlarge text, not all surrounding empty space.

## Rooms and navigation

The exact Tablet reference groups are:

| Room | Configured groups |
| --- | --- |
| Living Room | Scenes; Lights; Climate & Fan; Media |
| Kitchen | Scenes; Lights; Climate; Media; Sensors |
| Dining Room | Pendant light and thermostat |
| Master Bedroom | Lamps, thermostat, camera relay |
| Eugénie’s | Light and thermostat |
| Bathroom | Thermostat and speaker |
| Office | Lights/relays and thermostat |
| Guest Bedroom | Main lights, bed lamp, thermostat, TV |
| Outdoor | Lights, garage cover, lock, speakers |

Use a room surface about 640–840 logical pixels wide when space allows. Height fits content up to roughly 80–88% of the available window; only the body scrolls. On narrow windows use a near-full-width surface with the same title/back semantics. Equipment can use two columns when labels and controls fit; otherwise one. Scene buttons form a short wrapping group rather than enormous squares.

Equipment detail reuses the same scrim/surface. Show equipment name and room context with a named return such as “Salon”. Back/Escape/Android Back return to the room’s previous scroll position and originating control. The named close button dismisses the entire popup trail and restores the dashboard row. Pointer/touch opening does not paint an unsolicited keyboard focus ring; keyboard entry does show focus.

Other Lovelace views keep useful groupings: Climate is a compact thermostat list; At a glance is small observations grouped by purpose; Scenes is modest action tiles; irrigation places enable/status/duration together per zone; Garage groups actual cover/light/lock/media/camera controls. These do not require new hardcoded client pages.

## Thermostat family in Slint Kit Pro

Provide compact, inline-slider and expanded-detail variants. The closed presentation choice comes from the admitted source, not an installation ID.

Compact shows actual current temperature, operating mode and a separate setpoint. A missing target is “—”, never `0 °C`, a guessed current temperature or an empty unit. Off with no editable target can expose a prepared mode choice; the stepper stays disabled until a valid target action exists.

Detail is about 400–480 pixels wide with secondary current temperature, a large labelled setpoint, a 224–280-pixel arc/slider, explicit −/+ and supported mode/preset choices. Its intrinsic height normally fits within 640 pixels. The arc is not the only way to change a target; the same range can use a straight slider.

Range, step, units, modes and permissions come from admitted host models. Heating-only, heating/cooling and read-only configurations must make sense. No fake enabled modes/presets. Edited targets are visibly drafts during dragging; only committed intentions dispatch. Observed temperature and confirmed target remain unchanged until authoritative readback.

## Light and fan controls in Slint Kit Pro

A dimmer has an icon/name/state header and one horizontal control. Percentage remains readable independently of fill. Arrows use the supplied step; Home/End use its bounds. Pointer preview does not dispatch per pixel: release/keyboard commit emits one intention. Source, route or authorization changes cancel an uncommitted gesture.

A fan may use a percentage slider or an injected named-speed model. Do not hardcode four speeds, convert 75% into “3” by convention or equate zero to a power action unless that is the prepared intention. Light colour temperature is an explicitly supported secondary control. Power, level and details have distinct targets; a slider gesture must never also toggle power or open details.

## Media family in Slint Kit Pro

Compact has an optional 40–56-pixel thumbnail, source identity, track/artist when present, play/pause, supported previous/next and volume/mute. Idle/unavailable states remain quiet instead of retaining a giant empty artwork area. Playback state is not inferred from volume.

Detail is at most about 480 pixels wide, artwork around 160–192, readable metadata and supported transport/volume/seek/source/group controls. The host supplies/caches artwork; Pro never fetches arbitrary URLs. Text over artwork has a contrast-preserving surface/scrim. A TV without seek/volume support shows no dead controls; a native supported-action model determines available commands.

## States, feedback and command semantics

| State | Presentation |
| --- | --- |
| Coherent live | One concise state/value; no repeated “Disponible · À jour · Observé” diagnostics |
| Initially loading | Small local feedback after a meaningful delay; no shifting entire-board skeleton |
| Successfully empty resource | Configured empty label in its compact preview |
| Unavailable/unauthorized | Quiet inline status, retained identity, unavailable writes disabled |
| Stale/unknown | Value marked old/unknown; no write permission inferred |
| Submitted/acknowledged | Local pending indication; observed value remains authoritative |
| Refused/failed | Actionable message at the affected control, no whole-dashboard replacement |
| Uncertain outcome | Explicit uncertainty, no automatic resubmission |

Historical incidents are labelled as history. A recovered dashboard must not retain a prominent imperative implying its current coherent view still needs a capture. Do not clear an unversioned event on an unrelated Live callback: use correct cohort ownership or unambiguously historical wording.

Navigation, details, hover, focus and resize send no device commands. One gesture creates at most one prepared intention. The host validates exact target, capability, permission, range and current source/session generation. A held gesture cannot retarget after configuration/model-row replacement. Acknowledgment is not physical confirmation.

## Component boundaries and performance

Pro owns reusable presentation and local interaction state. Useful closed presentations are `row`, `tile`, `compact`, `detail`, with specific thermostat/dimmer/media variants when needed. Inject labels, icons, values, ranges, options, enabled/pending/availability state and opaque intention tokens. Components never store real IDs, fetch data, choose permissions, infer sources or execute commands.

Homeward owns source interpretation, authenticated transport, canonical bindings, resource readers and dispatch. The server owns composition, order, labels, selected sources, limits, presentation choices and routes. Secondary details exist only when admitted by the source, not as an invented installation route. Basic single-purpose controls stay in the free kit; automation entity/thermostat/media/dimmer families belong in Pro.

Preserve the existing tablet feel. Retain models/row identity, update only affected values, bound previews and virtualize long collections. Do not recreate the entire board, decode artwork, rebuild option models or block on network during dragging. Reduced Motion removes decoration; layout does not jump during acknowledgment/readback.

## Acceptance and review status

This file specifies the target; it does not claim these new presentations are already qualified. Validation uses the normal client and an isolated server with synthetic final providers. HA configuration may be read for reference; real devices/dashboard are not changed.

- At 1280 × 800 normal text, nine rooms form a compact list; simple sensors/switches have no hero-sized frames.
- Dashboard → room takes one activation. Exact room/section order is visible; open/close sends zero device writes.
- A direct step/toggle uses one gesture, a dimmer one committed drag; no extra Apply for a complete prepared intention.
- Current/target temperature, brightness, speed and playback remain distinct/truthful during pending/uncertain commands.
- New families demonstrate two representative configurations and customization; disabled, stale, missing-value and unsupported-operation states are covered.
- Source replacement during a held pointer/keyboard gesture cannot change its target. Back/Escape restores origin focus/scroll.
- Review board, room and detail in light/dark, wide/narrow, normal/200% text. Verify native Mac keyboard/focus and Android touch/Back. Screenshots alone establish neither interaction nor timing performance.

Attach actual source/binary/capture evidence after implementation. Explicitly identify an unsupported backend resource instead of displaying invented content to match a screenshot.

## Closed configuration additions

Native schema v3 admits `presentation: compact|detail` for `custom:homeward-thermostat`. Both variants bind the exact declared climate entity and fields; only a source action grants a detail destination. The default remains compact. Configured MDI icon names map to a finite bundled icon role; arbitrary SVG, code and downloads are not executed. Observation item icons remain per item. Unknown renderer enum values are inert.

The local native review uses the existing Ecobee scope amendment, real SQL/configuration/authentication/dashboard/list services, and explicit synthetic terminal devices. HA aliases outside that admitted manifest stay unresolved; they are never mapped to another equipment identity. Media/Calendar API availability and unresolved sources are recorded in the review exclusions. The private HA user-scoped list is excluded until an account mapping exists.
