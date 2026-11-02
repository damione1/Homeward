# Dashboard native v3

Dashboard composition is stored as an admitted Lovelace source in the existing
configuration generation. The same ConfigPlan / ConfigApply and workspace
reload path owns publication, rollback and validation. Dashboard reads use the
published `configactivation.Owner` generation; they do not read workspace files
or invent widgets from the installation catalogue.

A native source declares its trusted owner and stable URL path explicitly:

```yaml
homeward:
  profile: homeward-dashboard-3
  owner: configured-account-id
  url_path: home
views:
  - type: sections
    path: overview
    title: Maison
    max_columns: 4
    minimum_column_width: 280
    gap: 16
    sections:
      - type: grid
        cards:
          - type: custom:homeward-clock-weather
            title: Aujourd’hui
            timezone: America/Toronto
            weather_entity: weather.home
            grid_options:
              responsive_spans:
                - {columns: 4, span: 2}
                - {columns: 3, span: 1}
          - type: custom:homeward-room
            title: Salon
            room_id: living-room
            temperature: {entity: sensor.living_room_temperature, field: measurement}
            activity_entities: [light.living_room]
            active_states: ['on']
            action: /home/rooms
            action_label: Voir les pièces
          - type: custom:homeward-thermostat
            title: Chauffage
            entity: climate.living_room
            current_temperature:
              entity: climate.living_room
              field: current_temperature_celsius
            target_temperature:
              entity: climate.living_room
              field: setpoint_celsius
          - type: custom:homeward-agenda
            title: Agenda
            calendar_id: exact-calendar-entity-id
            timezone: America/Toronto
            window_seconds: 86400
            limit: 4
            empty_label: Aucun événement
          - type: custom:homeward-tasks
            title: Courses
            list_id: exact-list-id
            limit: 4
            empty_label: Rien à acheter
          - type: custom:homeward-security
            title: Sécurité
            entities: [lock.front_door, alarm_control_panel.home]
          - type: custom:homeward-observations
            items:
              - field: {entity: binary_sensor.presence, field: state}
                label: Présence
                active_states: ['on']
          - type: custom:homeward-navigation
            title: Pièces
            destination: /home/rooms
  - type: sections
    path: rooms
    title: Pièces
    sections: []
```

The IDs and aliases above illustrate syntax; they are not an installed catalogue.
All entity aliases must occur in the admitted registry manifest. Projection
resolves their exact canonical entity IDs in the current authorized view.
`calendar_id` is the exact canonical CalendarService entity ID; `list_id` is the
exact canonical ListService list ID. Missing or unauthorized data never selects
another source. The dashboard grants no entity, list, calendar or action rights.

The source owns view/card order, titles, bindings, visibility, navigation,
limits and responsive spans. Existing native recipes remain available, including
closed visibility predicates and navigation intentions. Native typography,
colors, focus behavior and touch metrics belong to the renderer.

`minimum_column_width` and `gap` are logical pixels, bounded by 4096 and 256 respectively. Minimum
width must be positive. A configured zero gap means no gap. V3 defaults are
supplied by the server: minimum width 280, gap 20, view maximum columns 4.
`grid_options.responsive_spans` contains at most 12 unique column counts in
1..12; each span is 1..columns. The span for the actual resolved column count
wins; otherwise the existing `grid_options.columns` fallback applies.

A navigation card without an `action_label` uses its configured `title` as the
button label. A navigation card must have a nonempty label. Duplicate navigation
intentions must agree; contradictory common actions and tap actions are rejected.

Every widget accepts `title`, `grid_options`, `visibility`, `tap_action`,
`action`, `action_label` and `empty_label`. `action` and navigation
`destination` are exact local configured routes resolved to native dashboard/node
identities by the server. An unresolved destination remains inert. No script,
HTML, template or downloaded widget implementation is executable.

Field keys are closed: `state`, `value`, `current_temperature_celsius`,
`setpoint_celsius`, `measurement`. Room temperature accepts only
`current_temperature_celsius` or `measurement`. Thermostat current and target
fields must refer to its configured entity and use
`current_temperature_celsius` and `setpoint_celsius` respectively.

Clock and agenda timezones are explicit named zones; environment-dependent
`Local` is rejected. Clock weather binding is optional; an omitted binding
means a clock without weather. Room temperature and activity bindings are
optional. Activity binds at most 4096 exact entities and at most 64 exact active
state literals; a nonempty activity binding requires active states. Security
and observation lists contain at most 64 entries. Agenda windows are 1..31,622,400
seconds; agenda and task limits are 1..1000. A complete source has at most 32
resource widgets and 4096 entity/collection bindings.

An explicit `views: []` produces an empty native layout. No default weather,
thermostat, room, task list or observation is inserted. Profile v1/v2 sources
retain their existing compilation behavior. V3 clients can consume the older
native forests; older clients receive inert diagnostics for unnegotiated v3
nodes. Raw source bytes remain behind the configuration read authorization;
DashboardService returns native data and metadata.

Existing sources without trusted `homeward.owner` metadata are not implicitly
assigned to the requesting account by the production service. Add the explicit
owner and stable `url_path` through the configuration publication workflow when
migrating those sources. The dashboard widget profile uses version 3.

Dashboard tasks and agenda readers have independent source and revision state
for each active widget. Choosing a list or calendar on another page cannot
retarget a dashboard preview. Task completion consumes the existing prepared
mutation once, retains the observed checkbox value until server readback, and
rejects stale widget/item/session generations. Agenda row selection can only
activate the widget’s configured navigation action; it creates no hidden editor.
An empty preview label is shown only for a coherent, fresh source. Unavailable,
unauthorized and stale sources carry an explicit status instead.

## Compact equipment and shopping presentations

In profile `homeward-dashboard-3`, a tile may select `presentation: summary` or
`controls` (the default). Summary changes presentation only: opening its exact
entity details does not dispatch a command. A thermostat widget also accepts
`presentation: summary`, alongside `compact` and `detail`. Its action still
provides an explicit configured destination.

Tasks accept `presentation: shopping` or `preview` (the default). Shopping uses
the exact configured list resource with checkbox-only rows; native Lists remains
the full editor. Unknown presentation values are unsupported. The example
review selects compact equipment and English source labels, with room grids
capped at three columns and a minimum useful column width.

### Window-width presentation conditions

The negotiated `ClientLayoutSupport.screen_visibility` capability admits this
closed Lovelace subset on cards and sections in `homeward-dashboard-3`:

```yaml
visibility:
  - condition: screen
    media_query: '(min-width: 768px) and (max-width: 1199px)'
```

A query contains one `min-width` or `max-width` clause, or one of each joined by
`and`. Bounds are integer logical window pixels, inclusive, at most65535;
`max-width` must be positive. The renderer uses the full window width, including
when the card is inside a narrow grid cell. A missing bound is unbounded. Several
screen conditions in the outer visibility list intersect their ranges. Screen
conditions cannot be nested in `and`/`or` expressions. Decimal widths, other units,
orientation, scripts and comma/or alternatives are not admitted. Unsupported or
contradictory rules produce inert diagnostics, including for clients that do not
advertise this capability.

This condition controls presentation only. Existing state/account predicates,
exact entity bindings, command admission and confirmation rules remain separate.
Resizing repacks the visible layout without issuing reads or changing authority.
