# Dashboard reference measurements

All values below come from the supplied HTML templates and the original bundle rendered by Chrome at device scale 1. They are reference dimensions, before adaptive layout and text scaling.

| Board | Width × height |
| --- | --- |
| Desktop home | 1440 × 1000 |
| Wall tablet home | 1280 × 800 |
| Mobile home | 390 × 844 |
| Salon room overlay | 1280 × 800 |
| Agenda and lists | 1440 × 1000 |
| Visual system | 1440 × 1480 |

## Geometry

| Part | Reference |
| --- | --- |
| Tablet rail | x12, y12, width76, height776, radius26 |
| Tablet main | x112; right inset24; header y18, height48 |
| Tablet status strip | y82, height36, item gap8 |
| Tablet page grid | 3 columns of368; section gap20 horizontally,24 vertically |
| Tablet overview | x112, width368; title y142; cards y172/300; each120 high |
| Tablet rooms | x500, width756; spans2 page columns; inner3 columns of248; gap8 |
| Tablet room cards | 3 ×3; y172/300/428; each248 ×120 |
| Tablet action tiles | x112/y428; each180 ×56, gap8 |
| Tablet lower row | titles y576; cards y606; Agenda x112, Épicerie x500, Sécurité x888 |
| Desktop rail | width80; outer inset16; radius28; gap28 |
| Desktop page grid | 4 columns; gap24; overview spans2 and rooms spans2 |
| Section inner grid | 12 columns; row56; gap8; height = rows ×56 + (rows−1) ×8 |
| Main card radius | 22; room20; single-row tile16; popup30 |
| Icon well | 36 or40; radius12 or13; section icon20 or22 |
| Navigation target | 52 ×52; radius16; glyph22 |
| Status chip | height36; radius999; leading icon well24 |

## Color

| Role | Light | Dark |
| --- | --- | --- |
| Canvas | `#F3F4F6` | `#0F1013` |
| Surface | `#FFFFFF` | `#1A1B20` |
| Inset | `#F7F7F9` | `#22242A` |
| Separator | `#E8E9EE` | `#2C2E35` |
| Text | `#17181C` | `#F2F3F5` |
| Secondary text | `#5E626B` | `#A3A7B0` |
| Accent | `#5A4FCF` | `#8B82F0` |
| Violet tint / ink | `#ECEAFB` / `#4B3FB8` | `#2A2650` / `#B9B2FF` |
| Heating tint / ink | `#FDE8DC` / `#A8431B` | `#3A261C` / `#F4B08E` |
| Lighting tint / ink | `#FCF1CF` / `#7A5600` | `#352D14` / `#EBCB6B` |
| Security/list tint / ink | `#DFF3E9` / `#1D6B49` | `#173025` / `#7FD3A8` |
| Weather/air/water tint / ink | `#E0EEFA` / `#1F5E99` | `#172A3B` / `#8CC2F0` |
| Alert tint / ink | `#FBE4E4` / `#A82828` | `#3A1C1F` / `#F2A0A0` |

Inactive room cards use a white surface and a neutral icon well. Active rooms use the lighting tint with darker brown state text. Standard elevation is `0 1px 2px rgba(16,18,27,.04), 0 8px 24px rgba(16,18,27,.06)`; active tinted cards have no shadow. Popup elevation is `0 24px 64px rgba(16,18,27,.22)`.

## Typography and assets

| Role | Size / line height | Weight |
| --- | --- | --- |
| Desktop clock | 52 /56 | 600 |
| Tablet clock | 44 /48 | 600 |
| Large value | 40 /44 | 600 |
| Desktop greeting | 28 /34 | 600 |
| Tablet greeting | 26 /32 | 600 |
| Section heading | 15 /22 | 600 |
| Body | 14 /20 | 500 or600 |
| Caption | 12 /16 | 400 |

Reference face: Inter with native system fallback. Numeric values request tabular digits. The bundle includes Inter and JetBrains Mono license notices; preserve those notices if extracting fonts. The reference icons are monochrome 24-unit SVG line paths, usually stroke1.75, with 18–26px view sizes. Use appropriately licensed cross-platform icons, or original equivalent paths.

## Breakpoints and adaptation

The fixed reference boards show an icon rail on both desktop and tablet, and a bottom bar on mobile. These are measured targets for the example server configuration, not hardcoded dashboard breakpoints. The server supplies container columns, minimum column widths, gaps and responsive spans; the native renderer resolves those constraints against available content width and text scale:

| Content width | Page columns | Top sections |
| --- | --- | --- |
| <620 × scale | 1 | Overview, then rooms |
| 620–899 × scale | 2 | Overview and rooms |
| 900–1239 × scale | 3 | Overview spans1; rooms spans2 |
| ≥1240 × scale | 4 | Overview and rooms each span2 |

Navigation changes below 600 × scale to a bottom bar; larger windows retain a floating icon rail (76 pixels on tablet, 80 on desktop). Slint Kit Pro also offers an explicitly enabled expanded navigation variant. Homeward preserves the reference’s rail.

Keep source reading order when sections reflow. At 200% text, allow extra grid rows and vertical scrolling; preserve minimum touch targets and avoid cropping labels or status text. Main actions, thermostat controls and list rows use 48 pixel targets; compact section actions keep their 32 pixel kit size. Media/energy content and destinations must be explicitly configured on the server; the client adds no placeholder section or live values.
