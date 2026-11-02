# Homeward dashboard design

## Current UX refinement

The current target is [UX-LOVELACE.md](UX-LOVELACE.md): the HTML remains the visual system, while Damien’s Tablet/Lovelace configuration supplies compact layout and interactions. Room summaries become compact rows, simple entities keep intrinsic heights, and detailed thermostat/media controls open on demand in one surface. The original reference geometry below does not require preserving the large room-card mosaic.

## Design plan

- **Archetype:** a calm home control dashboard with a persistent navigation rail on wide windows and a bottom navigation bar on narrow windows. Room selection opens its configured route or surface; equipment details reuse that surface with a clear way back. The server-owned dashboard remains the source of content and permissions.
- **Information architecture:** the application shell exposes its native pages. Dashboard sections, their titles, order, widgets, bindings, visibility and navigation come exclusively from the selected server document. The reference's Aperçu/Pièces/Agenda/Épicerie/Sécurité arrangement is an example configuration, never an additional client-owned overview.
- **Command map:** navigation selects configured destinations; room rows open details without issuing device commands; thermostat, dimmer and media controls emit typed intentions; list checkboxes emit existing list operations; Escape/system Back returns one level and restores the origin’s focus/scroll position. Existing keyboard focus and command handling stay with the application and public kit primitives.
- **Empty states:** render server-provided loading, disconnected, unauthorized and empty states. An absent weather, climate, room or calendar source must never be replaced by invented live values. Synthetic sources belong only to an explicitly isolated server qualification, rendered through the normal client.
- **Feeling:** calm and confident. Neutral surfaces, one saturated navigation accent and a quiet semantic pastel per card. State is conveyed with text and icons as well as color.

## Reference and adapted Mac principles

The source is Damien's `Homeward Dashboard.html` (six boards), measured directly from its HTML and inspected in Chrome. The reference inventory and tokens are in [KIT.md](../design/KIT.md). `scripts/capture-dashboard-reference.py` extracts the templates and can capture the original bundle's frames with an installed Puppeteer module.

Apply the `mac-design` and `layout-grid` skills as interaction and hierarchy rules across Slint: semantic palettes; aligned leading edges; stable keyboard focus; standard control behavior; motion reduction; deliberate empty states. The supplied reference is a custom home dashboard, so its measured radii and larger type intentionally override Mac control dimensions. Primary touch controls use 48 logical pixel targets; compact section actions retain the kit’s 32 pixel control size. Do not add glass to content cards. Use a native system font where a licensed bundled Inter asset is unavailable; do not borrow Apple-only icon assets for Android.

## Composition ownership

Basic web-like controls and layout foundations belong in Slint Kit. Injectable room summaries, thermostat cards, clock/weather displays, calendar/list previews and entity actions belong in Slint Kit Pro. Homeward owns server transport, stable installation/entity identities, credentials, command execution and synchronization. New components must accept themes/models and emit intentions instead of acquiring application state themselves.

## Review evidence

The client interprets the versioned native forest and its exact field/resource bindings. It adds no dashboard section when a server omits one and never selects the first weather, thermostat, calendar or task list. Typography, semantic colors, native focus/input behavior and bounded rendering remain client/kit responsibilities. Resource readers are independent of the editor pages and are cancelled when their configured owner changes. Unavailable sources differ from successfully read empty sources. See [the v3 contract](dashboard-native-v3.md) and [configuration qualification](DASHBOARD-CONFIGURATION.md).

Reference captures must use each board's declared logical viewport. Compare tablet geometry first, then typography, semantic colors, hover/pressed/focus states, resizing, light/dark and 200% text. A desktop rendering does not qualify Android touch, IME or accessibility.

The supplied room and visual-system boards have a JavaScript quoting error around `font-feature-settings: 'tnum'` in their dynamic data. Their static scaffolding renders, while repeated content is missing. Measure the source for these boards and document the limitation instead of claiming pixel parity with an empty reference.

For review, a separate generated copy can escape those three quotes and render the complete repeated content. Preserve the original source and label repaired captures explicitly. The original room reference is a centered 1080 × 704 popup at x100/y48 in its 1280 × 800 viewport, with three columns: Scènes/Climat, Lumières/Capteurs and Média. The new compact specification uses a 640–840-wide adaptive surface with one or two content columns and a bounded intrinsic height, preserving grouping while reducing empty space.

## Phone remote and responsive configuration

Screen conditions are presentation rules supplied by the server, not device identity or permissions. The supported Lovelace `screen` condition uses an inclusive window width range in logical pixels. It is applied before grid packing; resizing keeps stable opaque node identities and leaves no empty tracks for hidden cards. Unsupported conditions remain inert. Large text still reduces fitting columns independently of these ranges.

This installation uses mobile up to599px, tablet600–1023px and desktop from1024px. The phone opens on actionable Alarm and Garage summaries, followed by compact room entry points; weather and passive pills no longer occupy the first screen. Tablet adds climate and fan to the first row. Desktop keeps the information-rich composition. The same configured popups and typed detail routes serve every presentation. This choice is configuration data, not an app-owned household layout.

UX: Home opens on visible equipment state → one tap opens the exact alarm/garage detail → a named command changes the simulator state with a receipt → Back returns one level. Room summaries open compact entity lists; equipment controls remain in a single detail surface. Companion lists retain the searchable picker, per-list history and native keyboard. Test portrait/landscape, 200% app text, OS text size, safe areas and Android Back on the physical phone. Android TalkBack is a separate platform qualification: Slint1.18.1 currently has no Android semantic bridge; visual readability and labels alone do not satisfy it.

Inventory is an inspection destination inside Settings, rather than a daily shell tab. Back returns to Settings and keeps that section selected.

The companion grocery editor uses the Pro page layout: one outer scroll owner, a capture field above the collections, content-sized Items and Common items, and visible-row rendering for long histories. The host supplies the page viewport bounds; the component keeps row actions centered, suggestions spaced, and opaque list/item identities fenced across recycled rows. Shopping shortcuts remain checkbox-only consumption views.
