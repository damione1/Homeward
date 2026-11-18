//! Product adapter from a validated server forest to injected Pro presentation.
use crate::{
    ButtonIntent, ChoiceIcon, ChoiceRow, IconGlyph, ProAlarmCondition, ProAlarmOperation,
    ProCoverMotion, ProCoverOperation, ProDeviceRow, ProGestureAccess, ProLayoutColumns,
    ProLayoutFlow, ProLayoutHeading, ProLayoutInput, ProLayoutKind, ProLayoutMetricsPolicy,
    ProLayoutNode, ProLayoutSpan, ProLayoutSpanRule, ProLockOperation, ProMediaAction,
    ProMediaOperation, ProRecipeRow, ProRecipeStyle, ProTileAction, ProTileActionStyle, ProTileRow,
    ProTileTemplate, Tone,
};
use crate::{
    dashboard::{Layout, ProjectionRequest},
    dashboard_layout::{Dimension, HeadingStyle, Kind, Node, RecipeKind},
    device_view::{DeviceProjection, presentation::CardRow},
};
use slint::Model;
use std::collections::BTreeMap;

/// A tile or widget tint is a literal color, so the projection chooses the
/// pair that reads on the theme the board is painted under.
pub(crate) use crate::preferences::Appearance;
/// A fill and the ink drawn on it.
#[derive(Clone, Copy, Debug, PartialEq)]
pub(crate) struct TilePaint {
    pub tint: slint::Color,
    pub ink: slint::Color,
}
impl Appearance {
    /// Heating has no theme tone: the peach pair a heating tile or thermostat
    /// wears.
    pub(crate) fn heating(self) -> TilePaint {
        match self {
            Self::Light => TilePaint {
                tint: slint::Color::from_rgb_u8(0xFD, 0xE8, 0xDC),
                ink: slint::Color::from_rgb_u8(0xA8, 0x43, 0x1B),
            },
            Self::Dark => TilePaint {
                tint: slint::Color::from_rgb_u8(0x3A, 0x26, 0x1C),
                ink: slint::Color::from_rgb_u8(0xF4, 0xB0, 0x8E),
            },
        }
    }
    /// A lamp's observed color attenuated into a tile fill, with the color
    /// itself as ink (lifted toward white on dark so it stays readable).
    pub(crate) fn lamp(self, color: slint::Color) -> TilePaint {
        let white = slint::Color::from_rgb_u8(0xFF, 0xFF, 0xFF);
        match self {
            Self::Light => TilePaint {
                tint: color.mix(&white, 0.12),
                ink: color,
            },
            Self::Dark => TilePaint {
                tint: color.mix(&slint::Color::from_rgb_u8(0x0F, 0x10, 0x13), 0.22),
                ink: color.mix(&white, 0.6),
            },
        }
    }
}

pub(crate) struct BoardNode {
    data: Node,
    children: Vec<usize>,
    device: Option<CardRow>,
    square: bool,
    recipe: Option<crate::device_view::RecipePresentation>,
    widget: crate::dashboard_widget_reading::Reading,
}
#[derive(Clone, Copy, Default)]
pub(crate) enum BoardTarget {
    #[default]
    Clear,
    Retain,
    Home,
    Room,
    Popup,
}
#[derive(Default)]
pub(crate) struct Board {
    target: BoardTarget,
    appearance: Appearance,
    nodes: Vec<BoardNode>,
}
impl Board {
    pub fn project(
        layout: Option<&Layout>,
        request: &ProjectionRequest,
        devices: &DeviceProjection,
        view: Option<&home_client_cache::CachedView>,
    ) -> Self {
        let Some(layout) = layout.filter(|layout| layout.native.is_some()) else {
            return Self::default();
        };
        if request.detail.is_some() || !request.query.is_empty() {
            return Self {
                target: BoardTarget::Retain,
                appearance: Appearance::default(),
                nodes: Vec::new(),
            };
        }
        let target = match request
            .native
            .as_ref()
            .map(|destination| destination.presentation)
        {
            Some(home_client_model::dashboard::Presentation::Popup) => BoardTarget::Popup,
            Some(home_client_model::dashboard::Presentation::Page) => BoardTarget::Room,
            None if request.room.is_some() => BoardTarget::Room,
            None => BoardTarget::Home,
        };
        let Some(_) = request.native_root(layout) else {
            return Self {
                target,
                appearance: Appearance::default(),
                nodes: Vec::new(),
            };
        };
        let nodes = devices.native_nodes.iter().collect::<Vec<_>>();
        let square_children = nodes
            .iter()
            .filter(|node| node.kind == Kind::Grid && node.square == Some(true))
            .flat_map(|node| node.children.iter().cloned())
            .collect::<std::collections::BTreeSet<_>>();
        let indices = nodes
            .iter()
            .enumerate()
            .map(|(index, node)| (node.id.clone(), index))
            .collect::<BTreeMap<_, _>>();
        Self {
            target,
            appearance: Appearance::default(),
            nodes: nodes
                .into_iter()
                .map(|node| {
                    let device = devices.native_devices.get(&node.id).cloned().or_else(|| {
                        (!node.device_card())
                            .then(|| {
                                node.entity
                                    .as_ref()
                                    .and_then(|id| {
                                        devices.entities.iter().position(|entity| entity == id)
                                    })
                                    .and_then(|index| devices.rows.get(index))
                                    .cloned()
                            })
                            .flatten()
                    });
                    BoardNode {
                        widget: crate::dashboard_widget_reading::Reading::project(
                            node, layout, view,
                        ),
                        recipe: devices.recipes.get(&node.id).cloned(),
                        square: node.square.unwrap_or(false) || square_children.contains(&node.id),
                        data: node.clone(),
                        children: node
                            .children
                            .iter()
                            .filter_map(|id| indices.get(id).copied())
                            .collect(),
                        device,
                    }
                })
                .collect(),
        }
    }
    // A local directory changes arrangement only. Leaves retain their original
    // indices, opaque widget owners and exact configured navigation recipes.
    pub fn room_directory_rows(&self, mut rows: Vec<ProLayoutNode>) -> Vec<ProLayoutNode> {
        if !matches!(self.target, BoardTarget::Home) || rows.is_empty() {
            return Vec::new();
        }
        let mut pending = vec![(0usize, 0i32, 0i32)];
        let mut seen = Vec::new();
        let mut children = Vec::new();
        while let Some((index, inherited_min, inherited_max)) = pending.pop() {
            let node = &self.nodes[index];
            let minimum = inherited_min.max(node.data.viewport_min_width);
            let maximum = match (inherited_max, node.data.viewport_max_width) {
                (0, bound) | (bound, 0) => bound,
                (left, right) => left.min(right),
            };
            if maximum != 0 && minimum > maximum {
                continue;
            }
            if let Some(widget) = &node.data.widget
                && let crate::dashboard_widgets::Content::Room { room, .. } = &widget.content
            {
                let key = (room.clone(), widget.action.clone(), minimum, maximum);
                if !seen.contains(&key) {
                    seen.push(key);
                    children.push(ProLayoutInput {
                        index: index as i32,
                        viewport_min_width: minimum,
                        viewport_max_width: maximum,
                        rows: 1,
                        ..Default::default()
                    });
                }
            }
            pending.extend(
                node.children
                    .iter()
                    .rev()
                    .map(|child| (*child, minimum, maximum)),
            );
        }
        if children.is_empty() {
            return Vec::new();
        }
        rows[0] = ProLayoutNode {
            kind: ProLayoutKind::Container,
            flow: ProLayoutFlow::Grid,
            columns: 4,
            column_policy: ProLayoutColumns::Maximum,
            minimum_column_width: 136.,
            gap: 8.,
            metrics_policy: ProLayoutMetricsPolicy::Configured,
            children: slint::ModelRc::from(children.as_slice()),
            ..Default::default()
        };
        rows
    }
    pub fn target(&self) -> BoardTarget {
        self.target
    }
    /// The theme the rows are painted for: light until the host states the
    /// window's theme. It reaches tile tints and the thermostat widget.
    pub fn with_appearance(mut self, appearance: Appearance) -> Self {
        self.appearance = appearance;
        self
    }
    // The column count a child's span is measured against: a section or view
    // spans twelve unless configured, a grid its configured count. None while
    // the theme fits the columns, when no span can be judged compact.
    fn parent_columns(&self) -> Vec<Option<i32>> {
        let mut columns = vec![None; self.nodes.len()];
        for node in &self.nodes {
            let own = match node.data.kind {
                Kind::View | Kind::Section => Some(node.data.columns.map_or(12, i32::from)),
                Kind::Grid => node.data.columns.map(i32::from),
                Kind::HorizontalStack
                | Kind::VerticalStack
                | Kind::Popup
                | Kind::ChipGroup
                | Kind::Recipe
                | Kind::Heading
                | Kind::Tile
                | Kind::Widget
                | Kind::Unsupported => None,
            };
            for child in &node.children {
                columns[*child] = own;
            }
        }
        columns
    }
    fn token_base(&self) -> i32 {
        match self.target {
            BoardTarget::Home => 100_000,
            BoardTarget::Room => 200_000,
            BoardTarget::Popup => 300_000,
            BoardTarget::Clear | BoardTarget::Retain => 0,
        }
    }
    pub fn bindings(&self, generation: i32) -> Vec<crate::dashboard_widgets::Binding> {
        self.nodes
            .iter()
            .enumerate()
            .filter_map(|(index, node)| {
                Some(crate::dashboard_widgets::Binding {
                    token: self.token_base() + index as i32,
                    generation,
                    node: node.data.id.clone(),
                    config: node.data.widget.clone()?,
                })
            })
            .collect()
    }
    #[cfg(test)]
    pub fn rows(
        &self,
        previous: &slint::VecModel<ProLayoutNode>,
        icon: &IconGlyph,
    ) -> Vec<ProLayoutNode> {
        self.rows_with_generation(previous, icon, 0)
    }
    pub fn rows_with_generation(
        &self,
        previous: &slint::VecModel<ProLayoutNode>,
        icon: &IconGlyph,
        generation: i32,
    ) -> Vec<ProLayoutNode> {
        let parent_columns = self.parent_columns();
        self.nodes
            .iter()
            .enumerate()
            .map(|(index, node)| {
                let previous = previous.row_data(index);
                let data = &node.data;
                let selected_icon = crate::dashboard_icons::glyph(data.icon, icon);
                let icon = &selected_icon;
                let kind = match data.kind {
                    Kind::View
                    | Kind::Section
                    | Kind::Grid
                    | Kind::HorizontalStack
                    | Kind::VerticalStack
                    | Kind::Popup
                    | Kind::ChipGroup => ProLayoutKind::Container,
                    Kind::Heading => ProLayoutKind::Heading,
                    Kind::Widget => ProLayoutKind::Widget,
                    Kind::Tile => {
                        if node.device.is_some() {
                            if node
                                .device
                                .as_ref()
                                .is_some_and(|device| tile_supported(device, data))
                            {
                                ProLayoutKind::Tile
                            } else {
                                ProLayoutKind::Device
                            }
                        } else {
                            ProLayoutKind::Diagnostic
                        }
                    }
                    Kind::Recipe => match data.recipe_kind {
                        Some(RecipeKind::Select | RecipeKind::Alarm | RecipeKind::Entity) => {
                            if node.device.is_some() {
                                ProLayoutKind::Device
                            } else {
                                ProLayoutKind::Diagnostic
                            }
                        }
                        Some(
                            RecipeKind::Template
                            | RecipeKind::BubbleButton
                            | RecipeKind::TemplateChip
                            | RecipeKind::Button
                            | RecipeKind::Text,
                        ) => ProLayoutKind::Recipe,
                        Some(RecipeKind::BubblePopup | RecipeKind::Chips) | None => {
                            ProLayoutKind::Diagnostic
                        }
                    },
                    Kind::Unsupported => ProLayoutKind::Diagnostic,
                };
                let flow = match data.kind {
                    Kind::View | Kind::Section | Kind::Grid => ProLayoutFlow::Grid,
                    Kind::HorizontalStack | Kind::ChipGroup => ProLayoutFlow::Horizontal,
                    Kind::VerticalStack
                    | Kind::Popup
                    | Kind::Recipe
                    | Kind::Heading
                    | Kind::Tile
                    | Kind::Widget
                    | Kind::Unsupported => ProLayoutFlow::Vertical,
                };
                let children = node
                    .children
                    .iter()
                    .map(|index| {
                        let child = &self.nodes[*index].data;
                        let spans = child
                            .responsive_spans
                            .iter()
                            .map(|(columns, span)| ProLayoutSpanRule {
                                columns: i32::from(*columns),
                                span: i32::from(*span),
                            })
                            .collect::<Vec<_>>();
                        let previous_spans = previous
                            .as_ref()
                            .and_then(|node| {
                                node.children
                                    .iter()
                                    .find(|input| input.index == *index as i32)
                            })
                            .map(|input| input.spans);
                        let spans = previous_spans
                            .filter(|old| old.iter().eq(spans.iter().cloned()))
                            .unwrap_or_else(|| slint::ModelRc::from(spans.as_slice()));
                        let (span, count) = match child.column_span {
                            None | Some(Dimension::Auto) => (ProLayoutSpan::Automatic, 0),
                            Some(Dimension::Full) => (ProLayoutSpan::Full, 0),
                            Some(Dimension::Count(count)) => {
                                (ProLayoutSpan::Count, i32::from(count))
                            }
                        };
                        ProLayoutInput {
                            viewport_min_width: child.viewport_min_width,
                            viewport_max_width: child.viewport_max_width,
                            index: *index as i32,
                            column_span: span,
                            columns: count,
                            rows: match child.row_span {
                                Some(Dimension::Count(rows)) => i32::from(rows),
                                None | Some(Dimension::Auto | Dimension::Full) => 1,
                            },
                            spans,
                        }
                    })
                    .collect::<Vec<_>>();
                let mut device = node.device.clone().unwrap_or_default();
                device.summary_only = data.summary_only;
                if !data.title.is_empty() {
                    device.title = data.title.as_str().into();
                }
                let source = format!(
                    "{}:{}:{} · {} · {}",
                    data.source.file,
                    data.source.line,
                    data.source.column,
                    data.source.path,
                    data.source.variant
                );
                // This adapter supplies presentation only. The host installs
                // generation-sealed gesture bindings before profile2 is used.
                let access = |gesture| {
                    if let Some(recipe) = &node.recipe {
                        match recipe.access(gesture) {
                            crate::device_view::GestureAccess::Absent => ProGestureAccess::Absent,
                            crate::device_view::GestureAccess::Inactive => {
                                ProGestureAccess::Inactive
                            }
                            crate::device_view::GestureAccess::Local => ProGestureAccess::Enabled,
                            crate::device_view::GestureAccess::Command => {
                                ProGestureAccess::EnabledCommand
                            }
                        }
                    } else if data
                        .recipes
                        .as_ref()
                        .is_some_and(|recipes| recipes.interactions.contains_key(&gesture))
                    {
                        ProGestureAccess::Inactive
                    } else {
                        ProGestureAccess::Absent
                    }
                };
                let recipe = ProRecipeRow {
                    token: node.recipe.as_ref().map_or(-1, |recipe| recipe.token),
                    generation: node.recipe.as_ref().map_or(0, |recipe| recipe.generation),
                    notice: if node
                        .recipe
                        .as_ref()
                        .is_some_and(|recipe| recipe.unavailable)
                    {
                        "A configured interaction is unavailable.".into()
                    } else {
                        Default::default()
                    },
                    title: if data.title.is_empty() {
                        device.title.clone()
                    } else {
                        data.title.as_str().into()
                    },
                    secondary: data.secondary.as_str().into(),
                    source: source.as_str().into(),
                    style: if data.recipe_kind == Some(RecipeKind::TemplateChip) {
                        ProRecipeStyle::Chip
                    } else {
                        ProRecipeStyle::Card
                    },
                    tap: access(home_client_model::dashboard::Gesture::Tap),
                    hold: access(home_client_model::dashboard::Gesture::Hold),
                    double_tap: access(home_client_model::dashboard::Gesture::DoubleTap),
                    ..Default::default()
                };
                // Observation publications do not replace a container's
                // model. Only changed layout cells notify its delegates.
                let previous_children = previous
                    .as_ref()
                    .filter(|previous| {
                        previous.source == source.as_str()
                            && previous.kind == kind
                            && previous.flow == flow
                    })
                    .map(|previous| &previous.children);
                let children = crate::device_view::presentation::nested(
                    children.as_slice(),
                    previous_children,
                );
                let device_ui = device.ui(previous.as_ref().map(|node| &node.device), icon);
                // A scene is a compact chip on one row unless the layout
                // configures its rows; its tap runs it.
                let scene_tile = kind == ProLayoutKind::Tile
                    && node
                        .device
                        .as_ref()
                        .is_some_and(|device| device.kind == crate::ProDeviceKind::Scene);
                let tile_template = if scene_tile {
                    ProTileTemplate::Compact
                } else {
                    tile_template(data, node.square, parent_columns[index])
                };
                let rows = match tile_rows(data) {
                    0 if scene_tile => 1,
                    rows => rows,
                };
                // Only a tile reads the detail projection for its typed state.
                let state = if kind == ProLayoutKind::Tile {
                    tile_state(node.device.as_ref(), &device_ui)
                } else {
                    TileState::Plain
                };
                let tile = tile_row(
                    &device_ui,
                    &state,
                    tile_template,
                    rows,
                    self.appearance,
                    previous.as_ref().map(|node| &node.tile),
                );
                let mut widget = node.widget.ui(
                    data,
                    node.device.as_ref(),
                    previous.as_ref(),
                    (self.token_base() + index as i32, generation),
                    icon,
                    matches!(
                        recipe.tap,
                        ProGestureAccess::Enabled | ProGestureAccess::EnabledCommand
                    ),
                );
                // Reset on every publication: a widget starts from its previous
                // row, which must not keep the heating pair once heating stops.
                let paint = thermostat_paint(node.device.as_ref(), &widget, self.appearance);
                widget.tint = paint.map_or(slint::Color::default(), |paint| paint.tint);
                widget.ink = paint.map_or(slint::Color::default(), |paint| paint.ink);
                let title: slint::SharedString = if data.kind == Kind::View
                    || (index == 0 && matches!(self.target, BoardTarget::Popup))
                {
                    ""
                } else if kind == ProLayoutKind::Diagnostic && data.title.is_empty() {
                    data.source.variant.as_str()
                } else {
                    data.title.as_str()
                }
                .into();
                // A titled container is a section of the board: its title is a
                // section heading, its children sit two units apart and the
                // heading stands six units above them.
                let titled = kind == ProLayoutKind::Container && !title.is_empty();
                ProLayoutNode {
                    viewport_min_width: data.viewport_min_width,
                    viewport_max_width: data.viewport_max_width,
                    token: self.token_base() + index as i32,
                    generation,
                    widget,
                    kind,
                    flow,
                    title,
                    description: if kind == ProLayoutKind::Diagnostic
                        && data.kind == Kind::Unsupported
                    {
                        "Control unavailable for this item."
                    } else if kind == ProLayoutKind::Diagnostic
                        && data.entity.is_some()
                        && node.device.is_none()
                    {
                        "No authorized observation for this item."
                    } else {
                        data.diagnostic.as_str()
                    }
                    .into(),
                    source: source.into(),
                    heading: match data.heading_style {
                        HeadingStyle::Default if titled || data.gap.is_some() => {
                            ProLayoutHeading::Section
                        }
                        HeadingStyle::Default => ProLayoutHeading::Normal,
                        HeadingStyle::Title => ProLayoutHeading::Title,
                        HeadingStyle::Subtitle => ProLayoutHeading::Subtitle,
                    },
                    columns: data.columns.map_or(0, i32::from),
                    column_policy: match data.kind {
                        Kind::View | Kind::Section => ProLayoutColumns::Maximum,
                        Kind::Grid
                        | Kind::HorizontalStack
                        | Kind::VerticalStack
                        | Kind::Popup
                        | Kind::ChipGroup
                        | Kind::Recipe
                        | Kind::Heading
                        | Kind::Tile
                        | Kind::Widget
                        | Kind::Unsupported => ProLayoutColumns::Fixed,
                    },
                    span_base: if data.kind == Kind::Section { 12 } else { 0 },
                    minimum_column_width: data.minimum_column_width.unwrap_or(280) as f32,
                    gap: data
                        .gap
                        .map_or(if titled { 8. } else { 12. }, |gap| gap as f32),
                    section_gap: if titled { 24. } else { 0. },
                    metrics_policy: if titled
                        || data.gap.is_some()
                        || data.minimum_column_width.is_some()
                    {
                        ProLayoutMetricsPolicy::Configured
                    } else {
                        ProLayoutMetricsPolicy::Themed
                    },
                    square: node.square,
                    vertical: data.vertical.unwrap_or(false),
                    hide_state: data.hide_state.unwrap_or(false),
                    children,
                    device: device_ui,
                    tile,
                    tile_template,
                    tile_rows: rows,
                    recipe,
                    ..Default::default()
                }
            })
            .collect()
    }
}
// Rich custom editors and server-defined gesture recipes retain their own
// components. Every ordinary equipment tile uses the shared dashboard tile.
fn tile_supported(device: &crate::device_view::presentation::CardRow, node: &Node) -> bool {
    if device.modular {
        return false;
    }
    if node
        .recipes
        .as_ref()
        .is_some_and(|recipes| !recipes.interactions.is_empty())
    {
        return false;
    }
    match device.kind {
        crate::ProDeviceKind::Light
        | crate::ProDeviceKind::Switch
        | crate::ProDeviceKind::Binary
        | crate::ProDeviceKind::Measurement
        | crate::ProDeviceKind::Climate
        | crate::ProDeviceKind::Fan
        | crate::ProDeviceKind::Cover
        | crate::ProDeviceKind::Lock
        | crate::ProDeviceKind::Alarm
        | crate::ProDeviceKind::Media
        | crate::ProDeviceKind::Scene => true,
        crate::ProDeviceKind::Helper
        | crate::ProDeviceKind::Observation
        | crate::ProDeviceKind::Unsupported => false,
    }
}
// A tile narrower than a third of its grid shows its state alone; a square or
// vertical node takes the large template; everything else the rectangle.
fn tile_template(node: &Node, square: bool, parent_columns: Option<i32>) -> ProTileTemplate {
    if square || node.vertical == Some(true) {
        return ProTileTemplate::Large;
    }
    let span = match node.column_span {
        Some(Dimension::Count(span)) => Some(i32::from(span)),
        Some(Dimension::Auto | Dimension::Full) | None => None,
    };
    if let Some(span) = span
        && let Some(columns) = parent_columns
        && span * 3 <= columns
    {
        ProTileTemplate::Compact
    } else {
        ProTileTemplate::Rectangle
    }
}
// A configured row span; zero leaves the template's own row count.
fn tile_rows(node: &Node) -> i32 {
    match node.row_span {
        Some(Dimension::Count(rows)) => i32::from(rows),
        Some(Dimension::Auto | Dimension::Full) | None => 0,
    }
}

// Lucide glyphs (ISC license, from Slint Kit's bundled catalog) for the
// shortcut buttons a tile draws without labels.
mod glyph {
    use crate::IconGlyph;
    fn stroke(path: &str) -> IconGlyph {
        IconGlyph {
            stroke: path.into(),
            fill: "".into(),
            viewbox: 24.,
        }
    }
    pub(super) fn arrow_up() -> IconGlyph {
        stroke("M 5 12 L 12 5 L 19 12 M 12 19 L 12 5")
    }
    pub(super) fn arrow_down() -> IconGlyph {
        stroke("M 12 5 L 12 19 M 19 12 L 12 19 L 5 12")
    }
    pub(super) fn square() -> IconGlyph {
        stroke(
            "M 5 3 L 19 3 C 20.105 3 21 3.895 21 5 L 21 19 C 21 20.105 20.105 21 19 21 L 5 21 C 3.895 21 3 20.105 3 19 L 3 5 C 3 3.895 3.895 3 5 3 Z",
        )
    }
    pub(super) fn lock() -> IconGlyph {
        stroke(
            "M 5 11 L 19 11 C 20.105 11 21 11.895 21 13 L 21 20 C 21 21.105 20.105 22 19 22 L 5 22 C 3.895 22 3 21.105 3 20 L 3 13 C 3 11.895 3.895 11 5 11 Z M 7 11 L 7 7 C 7 4.239 9.239 2 12 2 C 14.761 2 17 4.239 17 7 L 17 11",
        )
    }
    pub(super) fn lock_open() -> IconGlyph {
        stroke(
            "M 5 11 L 19 11 C 20.105 11 21 11.895 21 13 L 21 20 C 21 21.105 20.105 22 19 22 L 5 22 C 3.895 22 3 21.105 3 20 L 3 13 C 3 11.895 3.895 11 5 11 Z M 7 11 L 7 7 C 6.997 4.431 8.942 2.278 11.498 2.02 C 14.053 1.762 16.389 3.483 16.9 6",
        )
    }
    pub(super) fn house() -> IconGlyph {
        stroke(
            "M 15 21 L 15 13 C 15 12.448 14.552 12 14 12 L 10 12 C 9.448 12 9 12.448 9 13 L 9 21 M 3 10 C 3 9.411 3.259 8.852 3.709 8.472 L 10.709 2.472 C 11.454 1.842 12.546 1.842 13.291 2.472 L 20.291 8.472 C 20.741 8.852 21 9.411 21 10 L 21 19 C 21 20.105 20.105 21 19 21 L 5 21 C 3.895 21 3 20.105 3 19 Z",
        )
    }
    pub(super) fn door_open() -> IconGlyph {
        stroke(
            "M 10 21 L 2 21 M 10 3 L 7 3 C 5.895 3 5 3.895 5 5 L 5 21 M 14 12 L 14.01 12 M 19 21 L 19 5 C 19 4.021 18.291 3.185 17.325 3.026 L 11.162 2.013 C 10.872 1.965 10.576 2.048 10.353 2.238 C 10.129 2.428 10 2.706 10 3 L 10 21 C 10 21.287 10.123 21.56 10.338 21.749 C 10.553 21.939 10.839 22.028 11.124 21.992 Z M 22 21 L 19 21",
        )
    }
    pub(super) fn moon() -> IconGlyph {
        stroke(
            "M 20.985 12.486 C 20.724 17.323 16.68 21.086 11.837 20.999 C 6.993 20.913 3.087 17.007 3 12.163 C 2.912 7.32 6.675 3.276 11.512 3.014 C 11.917 2.992 12.129 3.474 11.914 3.817 C 10.433 6.186 10.784 9.264 12.759 11.24 C 14.735 13.215 17.813 13.566 20.182 12.085 C 20.526 11.87 21.007 12.081 20.985 12.486",
        )
    }
    pub(super) fn plane() -> IconGlyph {
        stroke(
            "M 17.8 19.2 L 16 11 L 19.5 7.5 C 21 6 21.5 4 21 3 C 20 2.5 18 3 16.5 4.5 L 13 8 L 4.8 6.2 C 4.3 6.1 3.9 6.3 3.7 6.7 L 3.4 7.2 C 3.2 7.7 3.3 8.2 3.7 8.5 L 9 12 L 7 15 L 4 15 L 3 16 L 6 18 L 8 21 L 9 20 L 9 17 L 12 15 L 15.5 20.3 C 15.8 20.7 16.3 20.8 16.8 20.6 L 17.3 20.4 C 17.7 20.1 17.9 19.7 17.8 19.2 Z",
        )
    }
    pub(super) fn shield_off() -> IconGlyph {
        stroke(
            "M 2 2 L 22 22 M 5 5 C 4.448 5 4 5.448 4 6 L 4 13 C 4 18 7.5 20.5 11.67 21.94 C 11.886 22.02 12.122 22.024 12.34 21.95 C 14.69 21.13 16.82 19.98 18.24 18.24 M 9.309 3.652 C 9.995 3.258 10.642 2.799 11.24 2.28 C 11.678 1.906 12.322 1.906 12.76 2.28 C 14.51 3.81 17 5 19 5 C 19.552 5 20 5.448 20 6 L 20 13 C 20.001 13.423 19.974 13.845 19.92 14.264",
        )
    }
    pub(super) fn signal_low() -> IconGlyph {
        stroke("M 2 20 L 2.01 20 M 7 20 L 7 16")
    }
    pub(super) fn signal_medium() -> IconGlyph {
        stroke("M 2 20 L 2.01 20 M 7 20 L 7 16 M 12 20 L 12 12")
    }
    pub(super) fn signal_high() -> IconGlyph {
        stroke("M 2 20 L 2.01 20 M 7 20 L 7 16 M 12 20 L 12 12 M 17 20 L 17 8")
    }
    pub(super) fn sun() -> IconGlyph {
        stroke(
            "M 16 12 C 16 14.209 14.209 16 12 16 C 9.791 16 8 14.209 8 12 C 8 9.791 9.791 8 12 8 C 14.209 8 16 9.791 16 12 Z M 12 2 L 12 4 M 12 20 L 12 22 M 4.93 4.93 L 6.34 6.34 M 17.66 17.66 L 19.07 19.07 M 2 12 L 4 12 M 20 12 L 22 12 M 6.34 17.66 L 4.93 19.07 M 19.07 4.93 L 17.66 6.34",
        )
    }
}

/// A fan speed's glyph by its rank among the admitted speeds: the lowest
/// third low, the middle medium, the rest high (one speed alone is high,
/// two are low and high).
fn speed_glyph(index: usize, count: usize) -> IconGlyph {
    let count = count.max(1);
    let rank = (index * 3) / count;
    match rank {
        0 if count > 1 => glyph::signal_low(),
        1 if count > 2 => glyph::signal_medium(),
        _ => glyph::signal_high(),
    }
}
/// A level as a whole percentage of its domain; none without a domain.
fn level_percent(level: f32, minimum: f32, maximum: f32) -> Option<i32> {
    (maximum > minimum)
        .then(|| (((level - minimum) / (maximum - minimum)).clamp(0., 1.) * 100.).round() as i32)
}

/// What the equipment does now, as the detail projection reports it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum ClimateActivity {
    Idle,
    Heating,
    Cooling,
}
/// Where a positioned cover stands; unknown without a bounded observation.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum CoverPosition {
    Unknown,
    Open,
    Closed,
    Between,
}
#[derive(Clone, Copy, Debug, PartialEq)]
struct CoverState {
    motion: ProCoverMotion,
    position: CoverPosition,
}
/// What a tile paints beyond its power, read from the detail projection so
/// the tile and the detail panel agree on one typed source.
#[derive(Clone, Debug, PartialEq)]
enum TileState {
    Plain,
    Light {
        color: Option<slint::Color>,
        /// The observed colour's name ("Custom colour", a preset); empty
        /// without an observed colour.
        color_label: slint::SharedString,
    },
    Fan {
        speeds: Vec<ChoiceRow>,
    },
    Climate {
        activity: ClimateActivity,
        label: slint::SharedString,
        modes: Vec<ChoiceRow>,
        icons: Vec<ChoiceIcon>,
    },
    Cover {
        state: CoverState,
        /// The associated lock's observed state label; empty while the lock
        /// is absent or unknown.
        lock: slint::SharedString,
    },
    Alarm {
        condition: ProAlarmCondition,
    },
}
// The detail projection states heating itself; its activity tone tells the
// rest apart: information is cooling (cooling, drying or the fan alone, all
// painted cool), anything else idle.
fn climate_activity(detail: &crate::HomewardDetailRow) -> ClimateActivity {
    if detail.climate_heating {
        return ClimateActivity::Heating;
    }
    match detail.climate.activity_tone {
        Tone::Information => ClimateActivity::Cooling,
        Tone::Warning | Tone::Neutral | Tone::Accent | Tone::Success | Tone::Danger => {
            ClimateActivity::Idle
        }
    }
}
fn tile_state(card: Option<&CardRow>, device: &ProDeviceRow) -> TileState {
    use crate::ProDeviceKind as K;
    let Some(card) = card else {
        return TileState::Plain;
    };
    let detail = || crate::device_view::detail::project(card, device.clone(), None);
    match device.kind {
        K::Light => {
            let detail = detail();
            let light = &detail.light;
            let color = (light.hue_known && light.color.alpha() > 0).then_some(light.color);
            TileState::Light {
                color,
                color_label: if color.is_some() {
                    light.color_label.clone()
                } else {
                    slint::SharedString::default()
                },
            }
        }
        K::Fan => TileState::Fan {
            speeds: detail().fan.speeds.iter().collect(),
        },
        K::Climate => {
            let detail = detail();
            TileState::Climate {
                activity: climate_activity(&detail),
                label: detail.climate.activity.clone(),
                modes: detail.climate.modes.rows.iter().collect(),
                icons: detail.climate.modes.icons.iter().collect(),
            }
        }
        K::Cover => {
            let detail = detail();
            let cover = &detail.cover;
            let position = if !(detail.positioned && cover.position_known) {
                CoverPosition::Unknown
            } else if cover.position >= detail.position_maximum {
                CoverPosition::Open
            } else if cover.position <= detail.position_minimum {
                CoverPosition::Closed
            } else {
                CoverPosition::Between
            };
            let lock = &detail.associated_lock;
            TileState::Cover {
                state: CoverState {
                    motion: cover.motion,
                    position,
                },
                lock: match lock.state {
                    crate::ProCoverLock::Locked | crate::ProCoverLock::Unlocked => {
                        lock.state_label.clone()
                    }
                    crate::ProCoverLock::Absent | crate::ProCoverLock::Unknown => {
                        slint::SharedString::default()
                    }
                },
            }
        }
        K::Alarm => TileState::Alarm {
            condition: detail().alarm.condition,
        },
        K::Switch
        | K::Binary
        | K::Measurement
        | K::Helper
        | K::Lock
        | K::Observation
        | K::Unsupported
        | K::Media
        | K::Scene => TileState::Plain,
    }
}
// The thermostat widget wears the heating pair while the detail projection
// reports heating; any other activity keeps the widget's own tone.
fn thermostat_paint(
    card: Option<&CardRow>,
    widget: &crate::ProDashboardWidget,
    appearance: Appearance,
) -> Option<TilePaint> {
    let card = card?;
    if widget.kind != crate::ProDashboardWidgetKind::Thermostat {
        return None;
    }
    let detail = crate::device_view::detail::project(card, widget.thermostat.clone(), None);
    match climate_activity(&detail) {
        ClimateActivity::Heating => Some(appearance.heating()),
        ClimateActivity::Cooling | ClimateActivity::Idle => None,
    }
}
// The direction a cover already took, or cannot take, is disabled and the
// stop emphasised while it moves. An unknown state leaves every operation
// offered: the observed state decides, never a guess.
fn cover_action(row: &crate::ProCoverChoice, state: Option<CoverState>) -> ProTileAction {
    let glyph = match row.operation {
        ProCoverOperation::Open => glyph::arrow_up(),
        ProCoverOperation::Close => glyph::arrow_down(),
        ProCoverOperation::Stop => glyph::square(),
    };
    let (disabled, intent) = match state {
        None => (false, ButtonIntent::Secondary),
        Some(state) => match row.operation {
            ProCoverOperation::Open => (
                state.motion == ProCoverMotion::Opening || state.position == CoverPosition::Open,
                ButtonIntent::Secondary,
            ),
            ProCoverOperation::Close => (
                state.motion == ProCoverMotion::Closing || state.position == CoverPosition::Closed,
                ButtonIntent::Secondary,
            ),
            ProCoverOperation::Stop => match state.motion {
                ProCoverMotion::Idle => (false, ButtonIntent::Secondary),
                ProCoverMotion::Opening | ProCoverMotion::Closing => (false, ButtonIntent::Primary),
            },
        },
    };
    ProTileAction {
        token: row.token,
        label: row.label.clone(),
        glyph,
        intent,
        disabled,
    }
}
fn lock_action(row: &crate::ProLockChoice) -> ProTileAction {
    let glyph = match row.operation {
        ProLockOperation::Lock => glyph::lock(),
        ProLockOperation::Unlock => glyph::lock_open(),
    };
    ProTileAction {
        token: row.token,
        label: row.label.clone(),
        glyph,
        intent: ButtonIntent::Secondary,
        disabled: false,
    }
}
// Disarming is the destructive, emphasised action while the alarm is
// triggered and has nothing to do while it is disarmed; arming stays offered
// in every state but triggered, where disarming is the only shortcut.
fn alarm_action(
    row: &crate::ProAlarmChoice,
    condition: Option<ProAlarmCondition>,
) -> ProTileAction {
    let glyph = match row.operation {
        ProAlarmOperation::ArmHome => glyph::house(),
        ProAlarmOperation::ArmAway => glyph::door_open(),
        ProAlarmOperation::ArmNight => glyph::moon(),
        ProAlarmOperation::ArmVacation => glyph::plane(),
        ProAlarmOperation::Disarm => glyph::shield_off(),
    };
    let (disabled, intent) = match condition {
        None => (false, ButtonIntent::Secondary),
        Some(condition) => match row.operation {
            ProAlarmOperation::Disarm => match condition {
                ProAlarmCondition::Disarmed => (true, ButtonIntent::Secondary),
                ProAlarmCondition::Triggered => (false, ButtonIntent::Destructive),
                ProAlarmCondition::Arming
                | ProAlarmCondition::Armed
                | ProAlarmCondition::Pending
                | ProAlarmCondition::Unknown => (false, ButtonIntent::Secondary),
            },
            ProAlarmOperation::ArmHome
            | ProAlarmOperation::ArmAway
            | ProAlarmOperation::ArmNight
            | ProAlarmOperation::ArmVacation => (false, ButtonIntent::Secondary),
        },
    };
    ProTileAction {
        token: row.token,
        label: row.label.clone(),
        glyph,
        intent,
        disabled,
    }
}
fn tile_actions(device: &ProDeviceRow, state: &TileState) -> Vec<ProTileAction> {
    let cover = match state {
        TileState::Cover { state, .. } => Some(*state),
        TileState::Plain
        | TileState::Light { .. }
        | TileState::Fan { .. }
        | TileState::Climate { .. }
        | TileState::Alarm { .. } => None,
    };
    let alarm = match state {
        TileState::Alarm { condition } => Some(*condition),
        TileState::Plain
        | TileState::Light { .. }
        | TileState::Fan { .. }
        | TileState::Climate { .. }
        | TileState::Cover { .. } => None,
    };
    let triggered = alarm == Some(ProAlarmCondition::Triggered);
    device
        .cover_operations
        .iter()
        .map(|row| cover_action(&row, cover))
        .chain(device.lock_operations.iter().map(|row| lock_action(&row)))
        .chain(
            device
                .alarm_operations
                .iter()
                .filter(|row| !triggered || row.operation == ProAlarmOperation::Disarm)
                .map(|row| alarm_action(&row, alarm)),
        )
        .collect()
}
struct TileLook {
    tone: Tone,
    paint: Option<TilePaint>,
    active: bool,
}
// The tone a tile wears and whether it is lit: lamps and switches warm while
// on, fans cool, media and scenes accent while running, an alarm its
// condition, a thermostat its activity (heating in the peach pair). A lamp
// with an observed color wears that color instead of the tone.
fn tile_look(device: &ProDeviceRow, state: &TileState, appearance: Appearance) -> TileLook {
    use crate::ProDeviceKind as K;
    let on = device.power_known && device.power_on;
    let (tone, paint, active) = match state {
        TileState::Light { color, .. } => {
            (Tone::Warning, color.map(|color| appearance.lamp(color)), on)
        }
        TileState::Fan { .. } => (Tone::Information, None, on),
        TileState::Climate { activity, .. } => match activity {
            ClimateActivity::Heating => (Tone::Neutral, Some(appearance.heating()), true),
            ClimateActivity::Cooling => (Tone::Information, None, true),
            ClimateActivity::Idle => (Tone::Neutral, None, false),
        },
        TileState::Cover { .. } => (Tone::Neutral, None, false),
        TileState::Alarm { condition } => match condition {
            ProAlarmCondition::Disarmed => (Tone::Success, None, true),
            ProAlarmCondition::Armed => (Tone::Accent, None, true),
            ProAlarmCondition::Arming | ProAlarmCondition::Pending => (Tone::Warning, None, true),
            ProAlarmCondition::Triggered => (Tone::Danger, None, true),
            ProAlarmCondition::Unknown => (Tone::Neutral, None, false),
        },
        TileState::Plain => match device.kind {
            K::Light | K::Switch => (Tone::Warning, None, on),
            K::Fan => (Tone::Information, None, on),
            K::Media | K::Scene => (Tone::Accent, None, on),
            K::Climate
            | K::Alarm
            | K::Cover
            | K::Binary
            | K::Measurement
            | K::Helper
            | K::Lock
            | K::Observation
            | K::Unsupported => (Tone::Neutral, None, on),
        },
    };
    TileLook {
        tone,
        paint,
        active,
    }
}
// A compact tile has no visible control, so its tap is the equipment's
// default command: lamps and switches toggle, a fan with speeds moves to the
// next one, a player plays or pauses, a scene runs. Equipment whose change
// needs a choice opens its detail, as does every wider tile, which shows its
// own control. A scene runs from any template. A lamp drawn as a level fill
// on one row (`toggle_row`) has no power button: its tap is the power
// command. The tap is a plain command: the tile keeps showing the observed
// state.
fn tile_tap(
    device: &ProDeviceRow,
    template: ProTileTemplate,
    has_levels: bool,
    toggle_row: bool,
) -> crate::ProTileTap {
    use crate::{ProControlAccess, ProDeviceKind as K, ProTileTap as T};
    let commands = device.access == ProControlAccess::Allowed && !device.summary_only;
    if toggle_row && commands {
        return if device.can_power { T::Toggle } else { T::Open };
    }
    match template {
        ProTileTemplate::Compact if commands => match device.kind {
            K::Light | K::Switch => {
                if device.can_power {
                    T::Toggle
                } else {
                    T::Open
                }
            }
            K::Fan => {
                if has_levels {
                    T::NextLevel
                } else if device.can_power {
                    T::Toggle
                } else {
                    T::Open
                }
            }
            K::Media => T::PlayPause,
            K::Scene => T::Activate,
            K::Cover
            | K::Lock
            | K::Climate
            | K::Alarm
            | K::Measurement
            | K::Binary
            | K::Helper
            | K::Observation
            | K::Unsupported => T::Open,
        },
        ProTileTemplate::Compact | ProTileTemplate::Rectangle | ProTileTemplate::Large => {
            if commands && device.kind == K::Scene {
                T::Activate
            } else {
                T::Open
            }
        }
    }
}
/// The tile a synthetic card projects to, for the render tests' boards, at
/// the template's own row count.
#[cfg(test)]
pub(crate) fn synthetic_tile(
    card: &CardRow,
    device: &ProDeviceRow,
    template: ProTileTemplate,
    appearance: Appearance,
) -> ProTileRow {
    synthetic_tile_rows(card, device, template, 0, appearance)
}
/// The tile a synthetic card projects to over `rows` grid rows (zero: the
/// template's own), as the board projects a configured row span.
#[cfg(test)]
pub(crate) fn synthetic_tile_rows(
    card: &CardRow,
    device: &ProDeviceRow,
    template: ProTileTemplate,
    rows: i32,
    appearance: Appearance,
) -> ProTileRow {
    tile_row(
        device,
        &tile_state(Some(card), device),
        template,
        rows,
        appearance,
        None,
    )
}
// `rows` is the configured grid row span, zero for the template's own: only
// a configured single row draws a lamp as a level fill.
fn tile_row(
    device: &ProDeviceRow,
    state: &TileState,
    template: ProTileTemplate,
    rows: i32,
    appearance: Appearance,
    previous: Option<&ProTileRow>,
) -> ProTileRow {
    use crate::device_view::presentation::nested;
    use crate::{ProActionPhase as P, ProControlAccess, ProDeviceKind as K, ProTileControl as C};
    let previous =
        previous.filter(|row| row.token == device.token && row.generation == device.generation);
    let primary = device.values.iter().find(|value| {
        device.has_primary_value
            && value.token == device.primary_value_token
            && value.mode == crate::ProValueMode::Single
    });
    let on = device.power_known && device.power_on;
    let scene = device.kind == K::Scene;
    let media = device.kind == K::Media;
    let light = device.kind == K::Light;
    let large = template == ProTileTemplate::Large;
    let single_row = rows == 1;
    let actions = tile_actions(device, state);
    let choices: Vec<ChoiceRow> = device
        .choices
        .iter()
        .map(|row| ChoiceRow {
            token: row.token,
            label: row.label,
            selected: row.selected,
            disabled: false,
        })
        .collect();
    // Short labels from the detail projection where it has them (a fan's
    // speeds with a signal glyph by rank, a thermostat's modes with their
    // icons): their tokens index the same admitted choice list, so the host
    // resolves them unchanged.
    let (levels, level_icons): (Vec<ChoiceRow>, Vec<ChoiceIcon>) = match state {
        TileState::Fan { speeds } if !speeds.is_empty() => (
            speeds.clone(),
            speeds
                .iter()
                .enumerate()
                .map(|(index, row)| ChoiceIcon {
                    token: row.token,
                    glyph: speed_glyph(index, speeds.len()),
                })
                .collect(),
        ),
        TileState::Climate { modes, icons, .. } => (modes.clone(), icons.clone()),
        TileState::Fan { .. }
        | TileState::Plain
        | TileState::Light { .. }
        | TileState::Cover { .. }
        | TileState::Alarm { .. } => (choices, Vec::new()),
    };
    // The selected speed's rank among the admitted speeds ("2 of 3").
    let speed_rank = match state {
        TileState::Fan { speeds } => speeds
            .iter()
            .position(|row| row.selected)
            .map(|index| (index + 1, speeds.len())),
        TileState::Plain
        | TileState::Light { .. }
        | TileState::Climate { .. }
        | TileState::Cover { .. }
        | TileState::Alarm { .. } => None,
    };
    let alarm_triggered = matches!(
        state,
        TileState::Alarm {
            condition: ProAlarmCondition::Triggered
        }
    );
    let playing = media && on;
    // A player offers the opposite of its observed playback; the observed
    // state, not the press, flips the button. The card projection carries
    // no previous or next capability, so the tile offers neither.
    let transport = if media {
        vec![ProMediaAction {
            operation: if playing {
                ProMediaOperation::Pause
            } else {
                ProMediaOperation::Play
            },
            label: if playing { "Pause" } else { "Play" }.into(),
        }]
    } else {
        Vec::new()
    };
    // A dimmable lamp on one row is a level fill behind its row: no slider,
    // no power button, the tap is the power command.
    let fill_level = light && device.can_brightness && single_row;
    // A scene has no power button: its tap runs it.
    let control = if device.summary_only {
        C::None
    } else if media {
        C::Transport
    } else if device.can_brightness {
        C::Level
    } else if primary.is_some() && !levels.is_empty() {
        C::StepperLevels
    } else if primary.is_some() {
        C::Stepper
    } else if !actions.is_empty() {
        C::Actions
    } else if !levels.is_empty() {
        C::Levels
    } else if device.can_power && !scene {
        C::Toggle
    } else {
        C::None
    };
    let pending = matches!(
        device.phase,
        P::Preparing
            | P::Sending
            | P::Uncertain
            | P::Accepted
            | P::Dispatched
            | P::Acknowledged
            | P::Interrupted
    );
    let look = tile_look(device, state, appearance);
    // A lit lamp reads its brightness as its value ("70 %"); off, it keeps
    // the host's state word.
    let brightness: Option<slint::SharedString> = (light && on && device.brightness_known)
        .then(|| {
            level_percent(
                device.brightness_percent,
                device.brightness_minimum,
                device.brightness_maximum,
            )
        })
        .flatten()
        .map(|percent| format!("{percent} %").into());
    // A scene's value is its room, which the card does not carry: nothing.
    let value: slint::SharedString = if scene {
        slint::SharedString::default()
    } else if let Some(brightness) = brightness {
        brightness
    } else {
        format!(
            "{}{}{}",
            device.value,
            if device.unit.is_empty() { "" } else { " " },
            device.unit
        )
        .into()
    };
    // The detail is typed state, never the observation's freshness or
    // quality (those stay in the detail panel): a thermostat's activity, a
    // lamp's colour name, a fan's rank (its caption on a large tile), a
    // cover's lock.
    let detail: slint::SharedString = if !device.action_label.is_empty() {
        device.action_label.clone()
    } else if device.access == ProControlAccess::Denied {
        "Read-only".into()
    } else {
        match state {
            TileState::Climate { label, .. } => label.clone(),
            TileState::Light { color_label, .. } => color_label.clone(),
            TileState::Fan { .. } => speed_rank
                .filter(|_| !large)
                .map_or_else(slint::SharedString::default, |(rank, count)| {
                    format!("{rank} of {count}").into()
                }),
            TileState::Cover { lock, .. } => lock.clone(),
            TileState::Plain | TileState::Alarm { .. } => slint::SharedString::default(),
        }
    };
    let caption: slint::SharedString = speed_rank
        .filter(|_| large)
        .map_or_else(slint::SharedString::default, |(rank, count)| {
            format!("{rank} / {count}").into()
        });
    ProTileRow {
        token: device.token,
        generation: device.generation,
        title: device.title.clone(),
        value,
        detail,
        caption,
        // A compact thermostat leads with its reading over its activity.
        value_leads: template == ProTileTemplate::Compact && device.kind == K::Climate,
        glyph: device.glyph.clone(),
        tone: look.tone,
        tint: look
            .paint
            .map_or(slint::Color::default(), |paint| paint.tint),
        ink: look
            .paint
            .map_or(slint::Color::default(), |paint| paint.ink),
        active: look.active,
        // A lit rectangle over two rows keeps its resting surface and carries
        // the tone in its badge, power button and control; compact, large,
        // single-row and media tiles are tone-filled.
        badge_surface: template == ProTileTemplate::Rectangle && !single_row && !media,
        // The product's tone pairs keep caption contrast on their fills.
        tone_value: true,
        // A running scene is a solid accent chip; a player's badge stands in
        // for its artwork; a large switch is one hero power button.
        solid_active: scene,
        badge_filled: media,
        hero_power: device.kind == K::Switch && large && control == C::Toggle,
        power: if !device.power_known {
            crate::ProSwitchState::Unknown
        } else if device.power_on {
            crate::ProSwitchState::On
        } else {
            crate::ProSwitchState::Off
        },
        commands_disabled: device.access != ProControlAccess::Allowed
            || device.command_unavailable
            || pending,
        // Buttons retain separate exact capability intents; opening the
        // detail generates no transport call.
        tap: tile_tap(device, template, !levels.is_empty(), fill_level),
        hold_opens: true,
        control,
        // A dimmable lamp keeps its power button beside the slider.
        power_button: light && device.can_power && control == C::Level && !fill_level,
        level: device.brightness_percent,
        level_known: device.brightness_known,
        level_minimum: device.brightness_minimum,
        level_maximum: device.brightness_maximum,
        level_increment: device.brightness_step,
        level_fill: fill_level,
        level_glyph: if light && control == C::Level && !fill_level {
            glyph::sun()
        } else {
            IconGlyph::default()
        },
        levels_active: levels
            .iter()
            .find(|row| row.selected)
            .or_else(|| levels.first())
            .map_or(-1, |row| row.token),
        levels: nested(&levels, previous.map(|row| &row.levels)),
        level_icons: nested(&level_icons, previous.map(|row| &row.level_icons)),
        transport: nested(&transport, previous.map(|row| &row.transport)),
        playing,
        actions: nested(&actions, previous.map(|row| &row.actions)),
        // A triggered alarm's lone disarm is a labelled, full-width button.
        action_style: if alarm_triggered {
            ProTileActionStyle::Labelled
        } else {
            ProTileActionStyle::Glyphs
        },
        stepper_value: primary
            .as_ref()
            .map(|value| format!("{} {}", value.lower, value.suffix).into())
            .unwrap_or_default(),
        can_increment: primary
            .as_ref()
            .is_some_and(|value| value.can_step && value.lower_position < 1.),
        can_decrement: primary
            .as_ref()
            .is_some_and(|value| value.can_step && value.lower_position > 0.),
        ..Default::default()
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::dashboard_layout::NodeId;

    #[test]
    fn rooms_directory_preserves_ancestor_screen_rules_and_deduplicates_exact_copies() {
        use crate::dashboard_widgets::{Content, Widget};
        use home_client_model::dashboard::{Destination, Presentation};
        let mut native = crate::dashboard_layout::NativeLayout::try_from(
            &crate::dashboard_layout::tests::extended_forest(),
        )
        .unwrap();
        let template = native
            .nodes
            .remove(&NodeId::parse("tile").unwrap())
            .unwrap();
        let destination = Destination {
            dashboard: home_client_model::api::DashboardName::try_from(
                "homes/synthetic/dashboards/configured",
            )
            .unwrap(),
            node: NodeId::parse("popup").unwrap(),
            presentation: Presentation::Popup,
        };
        let mut room = template.clone();
        room.kind = Kind::Widget;
        room.entity = None;
        room.recipes = None;
        room.features.clear();
        room.widget = Some(Widget {
            content: Content::Room {
                room: crate::dashboard::RoomId::parse("same-room").unwrap(),
                temperature: None,
                activity: Vec::new(),
            },
            action: Some(destination),
            action_label: String::new(),
            empty_label: String::new(),
        });
        let mut duplicate = room.clone();
        duplicate.id = NodeId::parse("copy").unwrap();
        let mut mobile = room.clone();
        mobile.id = NodeId::parse("mobile-copy").unwrap();
        mobile.viewport_max_width = 599;
        let mut section = template.clone();
        section.id = NodeId::parse("desktop-section").unwrap();
        section.kind = Kind::Section;
        section.entity = None;
        section.children = vec![room.id.clone(), duplicate.id.clone()];
        section.viewport_min_width = 600;
        let mut root = section.clone();
        root.id = NodeId::parse("view").unwrap();
        root.kind = Kind::View;
        root.viewport_min_width = 0;
        root.children = vec![section.id.clone(), mobile.id.clone()];
        let mut layout = crate::dashboard::fixture();
        layout.native = Some(crate::dashboard_layout::NativeLayout {
            views: vec![root.id.clone()],
            nodes: [
                root.clone(),
                section.clone(),
                room.clone(),
                duplicate.clone(),
                mobile.clone(),
            ]
            .into_iter()
            .map(|node| (node.id.clone(), node))
            .collect(),
        });
        let devices = DeviceProjection {
            native_nodes: vec![root, section, room, duplicate, mobile],
            ..Default::default()
        };
        let board = Board::project(Some(&layout), &ProjectionRequest::default(), &devices, None);
        let rows = board.rows(&Default::default(), &Default::default());
        let directory = board.room_directory_rows(rows.clone());
        let inputs = directory[0].children.iter().collect::<Vec<_>>();
        assert_eq!(
            inputs.len(),
            2,
            "same source room and destination is shown once per screen range"
        );
        assert_eq!(
            (inputs[0].viewport_min_width, inputs[0].viewport_max_width),
            (600, 0)
        );
        assert_eq!(
            (inputs[1].viewport_min_width, inputs[1].viewport_max_width),
            (0, 599)
        );
        for input in inputs {
            let index = input.index as usize;
            assert_eq!(directory[index].token, rows[index].token);
            assert_eq!(directory[index].widget, rows[index].widget);
        }
    }
    #[test]
    fn observed_values_refresh_without_replacing_layout_models_and_real_cells_still_update() {
        let mut layout = crate::dashboard::fixture();
        layout.native = Some(
            crate::dashboard_layout::NativeLayout::try_from(
                &crate::dashboard_layout::tests::extended_forest(),
            )
            .unwrap(),
        );
        let native = layout.native.as_ref().unwrap();
        let mut devices = DeviceProjection {
            native_nodes: native
                .ordered_nodes(native.root(None).unwrap())
                .into_iter()
                .cloned()
                .collect(),
            entities: vec![
                home_client_model::api::EntityName::try_from(
                    crate::dashboard_layout::tests::ENTITY,
                )
                .unwrap(),
            ],
            rows: vec![crate::device_view::presentation::CardRow {
                value: "First observation".into(),
                ..Default::default()
            }],
            ..Default::default()
        };
        let request = ProjectionRequest::default();
        devices
            .native_devices
            .insert(NodeId::parse("tile").unwrap(), devices.rows[0].clone());
        let first_board = Board::project(Some(&layout), &request, &devices, None);
        let first = first_board.rows(&slint::VecModel::default(), &IconGlyph::default());
        let previous = slint::VecModel::from(first.clone());
        devices.rows[0].value = "New observation".into();
        devices
            .native_devices
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap()
            .value = "New observation".into();
        let next = Board::project(Some(&layout), &request, &devices, None)
            .rows(&previous, &IconGlyph::default());
        for (before, after) in first.iter().zip(&next) {
            if before.kind == ProLayoutKind::Container {
                assert_eq!(
                    before.children, after.children,
                    "a value publication must preserve the container model"
                );
            }
        }
        let tile = first_board
            .nodes
            .iter()
            .position(|node| node.data.id.as_str() == "tile")
            .unwrap();
        assert_eq!(next[tile].device.value, "New observation");
        let grid = first_board
            .nodes
            .iter()
            .position(|node| node.data.id.as_str() == "grid")
            .unwrap();
        devices
            .native_nodes
            .iter_mut()
            .find(|node| node.id.as_str() == "tile")
            .unwrap()
            .column_span = Some(Dimension::Count(2));
        let changed = Board::project(Some(&layout), &request, &devices, None)
            .rows(&previous, &IconGlyph::default());
        assert_eq!(changed[grid].children, first[grid].children);
        assert_eq!(
            changed[grid].children.row_data(0).unwrap().columns,
            2,
            "a changed configured span must still reach the existing model"
        );
    }

    #[test]
    fn extended_projection_preserves_plain_text_chips_and_hides_popup_children() {
        let mut layout = crate::dashboard::fixture();
        layout.native = Some(
            crate::dashboard_layout::NativeLayout::try_from(
                &crate::dashboard_layout::tests::extended_forest(),
            )
            .unwrap(),
        );
        let native = layout.native.as_ref().unwrap();
        let devices = DeviceProjection {
            native_nodes: native
                .ordered_nodes(native.root(None).unwrap())
                .into_iter()
                .cloned()
                .collect(),
            ..Default::default()
        };
        let board = Board::project(Some(&layout), &ProjectionRequest::default(), &devices, None);
        let rows = board.rows(&slint::VecModel::default(), &IconGlyph::default());
        assert_eq!(rows.len(), 10);
        assert!(
            board
                .nodes
                .iter()
                .all(|node| !matches!(node.data.id.as_str(), "popup" | "popup-tile"))
        );
        let index = |id| {
            board
                .nodes
                .iter()
                .position(|node| node.data.id.as_str() == id)
                .unwrap()
        };
        let recipe = &rows[index("recipe")];
        assert_eq!(recipe.kind, ProLayoutKind::Recipe);
        assert_eq!(
            recipe.recipe.title,
            "Plain text\n<script>not executed</script>"
        );
        assert_eq!(recipe.recipe.secondary, "Server-projected text");
        assert_eq!(
            recipe.recipe.tap,
            ProGestureAccess::Inactive,
            "no binding conveys authority before host composition"
        );
        assert_eq!(recipe.recipe.hold, ProGestureAccess::Absent);
        assert!(recipe.recipe.source.contains("synthetic.yaml"));
        assert_eq!(rows[index("tile")].kind, ProLayoutKind::Diagnostic);
        assert_eq!(rows[index("chips")].flow, ProLayoutFlow::Horizontal);
        assert_eq!(rows[index("chip")].recipe.style, ProRecipeStyle::Chip);
        assert_eq!(rows[index("vertical")].flow, ProLayoutFlow::Vertical);
        for (index, node) in board.nodes.iter().enumerate() {
            assert_eq!(rows[index].children.row_count(), node.children.len());
            assert!(node.children.iter().all(|child| *child < rows.len()));
        }
    }
    #[test]
    fn new_tiles_keep_fractional_domains_and_separate_safe_navigation_from_commands() {
        let mut device = crate::ProDeviceRow {
            token: 0,
            generation: 9,
            kind: crate::ProDeviceKind::Light,
            title: "Synthetic lamp".into(),
            can_brightness: true,
            brightness_known: true,
            brightness_percent: 0.2,
            brightness_minimum: 0.1,
            brightness_maximum: 0.3,
            brightness_step: 0.1,
            access: crate::ProControlAccess::Allowed,
            ..Default::default()
        };
        let tile = tile_row(
            &device,
            &TileState::Plain,
            ProTileTemplate::Rectangle,
            0,
            Appearance::Light,
            None,
        );
        assert_eq!(tile.tap, crate::ProTileTap::Open);
        assert_eq!(tile.control, crate::ProTileControl::Level);
        assert_eq!(tile.tone, Tone::Warning);
        assert!(
            !tile.power_button,
            "a lamp without power keeps the slider alone"
        );
        assert_eq!(
            (tile.level_minimum, tile.level_maximum, tile.level_increment),
            (0.1, 0.3, 0.1)
        );
        assert!(!tile.commands_disabled);
        device.access = crate::ProControlAccess::Denied;
        let denied = tile_row(
            &device,
            &TileState::Plain,
            ProTileTemplate::Rectangle,
            0,
            Appearance::Light,
            Some(&tile),
        );
        assert!(denied.commands_disabled);
        assert_eq!(
            denied.tap,
            crate::ProTileTap::Open,
            "read-only detail remains navigable"
        );
        assert_eq!(denied.token, 0);
        assert_eq!(denied.generation, 9);
    }
    #[test]
    fn compact_tiles_take_the_default_command_and_wider_tiles_open() {
        use crate::{ProControlAccess, ProDeviceKind, ProTileControl, ProTileTap};
        let row = |kind: ProDeviceKind, access: ProControlAccess| ProDeviceRow {
            kind,
            access,
            can_power: true,
            power_known: true,
            power_on: true,
            ..Default::default()
        };
        let tile = |device: &ProDeviceRow, template: ProTileTemplate| {
            tile_row(
                device,
                &TileState::Plain,
                template,
                0,
                Appearance::Light,
                None,
            )
        };
        let lamp = row(ProDeviceKind::Light, ProControlAccess::Allowed);
        let compact = tile(&lamp, ProTileTemplate::Compact);
        assert_eq!(compact.tap, ProTileTap::Toggle);
        assert_eq!(compact.tone, Tone::Warning);
        assert!(compact.active, "a lamp that is on is lit");
        assert_eq!(
            compact.tint.alpha(),
            0,
            "no observed color, the tone paints"
        );
        assert_eq!(
            tile(&lamp, ProTileTemplate::Rectangle).tap,
            ProTileTap::Open
        );
        assert_eq!(
            tile(
                &row(ProDeviceKind::Light, ProControlAccess::Denied),
                ProTileTemplate::Compact
            )
            .tap,
            ProTileTap::Open,
            "a read-only lamp opens its detail instead of toggling"
        );
        let scene = tile(
            &row(ProDeviceKind::Scene, ProControlAccess::Allowed),
            ProTileTemplate::Rectangle,
        );
        assert_eq!(
            scene.tap,
            ProTileTap::Activate,
            "a scene runs from any template"
        );
        assert_eq!(scene.tone, Tone::Accent);
        let player = tile(
            &row(ProDeviceKind::Media, ProControlAccess::Allowed),
            ProTileTemplate::Compact,
        );
        assert_eq!(player.tap, ProTileTap::PlayPause);
        assert_eq!(player.control, ProTileControl::Transport);
        assert!(player.playing);
        assert_eq!(
            player.transport.row_data(0).map(|action| action.operation),
            Some(ProMediaOperation::Pause),
            "a playing player offers pause"
        );
    }
    #[test]
    fn cover_shortcuts_carry_glyphs_and_follow_the_observed_motion() {
        use crate::{ProControlAccess, ProCoverChoice, ProDeviceKind, ProTileControl};
        let cover = ProDeviceRow {
            kind: ProDeviceKind::Cover,
            access: ProControlAccess::Allowed,
            cover_operations: slint::ModelRc::from(
                [
                    ProCoverChoice {
                        token: 0,
                        label: "Open".into(),
                        operation: ProCoverOperation::Open,
                    },
                    ProCoverChoice {
                        token: 1,
                        label: "Stop".into(),
                        operation: ProCoverOperation::Stop,
                    },
                    ProCoverChoice {
                        token: 2,
                        label: "Close".into(),
                        operation: ProCoverOperation::Close,
                    },
                ]
                .as_slice(),
            ),
            ..Default::default()
        };
        let tile = |state: CoverState| {
            tile_row(
                &cover,
                &TileState::Cover {
                    state,
                    lock: Default::default(),
                },
                ProTileTemplate::Rectangle,
                0,
                Appearance::Light,
                None,
            )
        };
        let opening = tile(CoverState {
            motion: ProCoverMotion::Opening,
            position: CoverPosition::Between,
        });
        assert_eq!(opening.control, ProTileControl::Actions);
        assert_eq!(opening.action_style, ProTileActionStyle::Glyphs);
        let actions = opening.actions.iter().collect::<Vec<_>>();
        assert!(actions.iter().all(|action| !action.glyph.stroke.is_empty()));
        assert!(actions[0].disabled, "the direction taken is disabled");
        assert_eq!(
            actions[1].intent,
            ButtonIntent::Primary,
            "stop is emphasised while moving"
        );
        assert!(!actions[2].disabled);
        let open = tile(CoverState {
            motion: ProCoverMotion::Idle,
            position: CoverPosition::Open,
        });
        let actions = open.actions.iter().collect::<Vec<_>>();
        assert!(actions[0].disabled, "a fully open cover cannot open");
        assert_eq!(actions[1].intent, ButtonIntent::Secondary);
        assert!(
            !actions[1].disabled,
            "stop stays offered: the observed motion may lag"
        );
        assert!(!actions[2].disabled);
    }
    #[test]
    fn heating_wears_the_peach_pair_and_alarm_conditions_their_tones() {
        use crate::ProDeviceKind;
        let thermostat = ProDeviceRow {
            kind: ProDeviceKind::Climate,
            ..Default::default()
        };
        let climate = |activity: ClimateActivity, label: &str| TileState::Climate {
            activity,
            label: label.into(),
            modes: Vec::new(),
            icons: Vec::new(),
        };
        let light = tile_row(
            &thermostat,
            &climate(ClimateActivity::Heating, "Heating"),
            ProTileTemplate::Compact,
            0,
            Appearance::Light,
            None,
        );
        assert_eq!(light.tint, Appearance::Light.heating().tint);
        assert_eq!(light.ink, Appearance::Light.heating().ink);
        assert!(light.active && light.value_leads);
        assert_eq!(light.detail, "Heating");
        let dark = tile_row(
            &thermostat,
            &climate(ClimateActivity::Heating, "Heating"),
            ProTileTemplate::Compact,
            0,
            Appearance::Dark,
            None,
        );
        assert_eq!(dark.tint, Appearance::Dark.heating().tint);
        let idle = tile_row(
            &thermostat,
            &climate(ClimateActivity::Idle, ""),
            ProTileTemplate::Rectangle,
            0,
            Appearance::Light,
            None,
        );
        assert!(!idle.active && !idle.value_leads);
        assert_eq!(idle.tint.alpha(), 0);
        let alarm = ProDeviceRow {
            kind: ProDeviceKind::Alarm,
            ..Default::default()
        };
        for (condition, tone, lit) in [
            (ProAlarmCondition::Disarmed, Tone::Success, true),
            (ProAlarmCondition::Armed, Tone::Accent, true),
            (ProAlarmCondition::Triggered, Tone::Danger, true),
            (ProAlarmCondition::Unknown, Tone::Neutral, false),
        ] {
            let tile = tile_row(
                &alarm,
                &TileState::Alarm { condition },
                ProTileTemplate::Rectangle,
                0,
                Appearance::Light,
                None,
            );
            assert_eq!(tile.tone, tone);
            assert_eq!(tile.active, lit);
        }
    }
    #[test]
    fn typed_state_feeds_the_value_the_detail_and_the_shortcuts() {
        use crate::{ProAlarmChoice, ProControlAccess, ProDeviceKind, ProTileControl, ProTileTap};
        let lamp = ProDeviceRow {
            kind: ProDeviceKind::Light,
            access: ProControlAccess::Allowed,
            can_power: true,
            can_brightness: true,
            power_known: true,
            power_on: true,
            brightness_known: true,
            brightness_percent: 70.,
            brightness_minimum: 0.,
            brightness_maximum: 100.,
            brightness_step: 1.,
            value: "On".into(),
            quality: "Current observation".into(),
            ..Default::default()
        };
        let state = TileState::Light {
            color: None,
            color_label: "".into(),
        };
        let two_rows = tile_row(
            &lamp,
            &state,
            ProTileTemplate::Rectangle,
            2,
            Appearance::Light,
            None,
        );
        assert_eq!(two_rows.value, "70 %", "a lit lamp reads its brightness");
        assert_eq!(
            two_rows.detail, "",
            "the observation quality stays off the tile"
        );
        assert!(two_rows.badge_surface && two_rows.tone_value && two_rows.power_button);
        assert!(!two_rows.level_fill);
        assert!(
            !two_rows.level_glyph.stroke.is_empty(),
            "a slider leads with the sun"
        );
        assert_eq!(two_rows.tap, ProTileTap::Open);
        let one_row = tile_row(
            &lamp,
            &state,
            ProTileTemplate::Rectangle,
            1,
            Appearance::Light,
            None,
        );
        assert!(one_row.level_fill && !one_row.power_button && !one_row.badge_surface);
        assert_eq!(
            one_row.tap,
            ProTileTap::Toggle,
            "a level fill toggles on tap"
        );
        assert_eq!(one_row.control, ProTileControl::Level);
        let fan = ProDeviceRow {
            kind: ProDeviceKind::Fan,
            access: ProControlAccess::Allowed,
            value: "Medium".into(),
            ..Default::default()
        };
        let speeds = ["Low", "Medium", "High"]
            .into_iter()
            .enumerate()
            .map(|(index, label)| ChoiceRow {
                token: index as i32,
                label: label.into(),
                selected: index == 1,
                disabled: false,
            })
            .collect::<Vec<_>>();
        let rectangle = tile_row(
            &fan,
            &TileState::Fan {
                speeds: speeds.clone(),
            },
            ProTileTemplate::Rectangle,
            0,
            Appearance::Light,
            None,
        );
        assert_eq!(rectangle.detail, "2 of 3");
        assert_eq!(rectangle.caption, "");
        assert_eq!(
            rectangle.level_icons.row_count(),
            3,
            "every speed has a glyph"
        );
        let large = tile_row(
            &fan,
            &TileState::Fan { speeds },
            ProTileTemplate::Large,
            0,
            Appearance::Light,
            None,
        );
        assert_eq!(large.caption, "2 / 3");
        assert_eq!(
            large.detail, "",
            "the caption carries the rank on a large tile"
        );
        let alarm = ProDeviceRow {
            kind: ProDeviceKind::Alarm,
            access: ProControlAccess::Allowed,
            alarm_operations: slint::ModelRc::from(
                [
                    ProAlarmChoice {
                        token: 0,
                        label: "Arm Away".into(),
                        operation: ProAlarmOperation::ArmAway,
                    },
                    ProAlarmChoice {
                        token: 1,
                        label: "Disarm".into(),
                        operation: ProAlarmOperation::Disarm,
                    },
                ]
                .as_slice(),
            ),
            ..Default::default()
        };
        let triggered = tile_row(
            &alarm,
            &TileState::Alarm {
                condition: ProAlarmCondition::Triggered,
            },
            ProTileTemplate::Large,
            0,
            Appearance::Light,
            None,
        );
        assert_eq!(triggered.action_style, ProTileActionStyle::Labelled);
        let actions = triggered.actions.iter().collect::<Vec<_>>();
        assert_eq!(actions.len(), 1, "only disarm while triggered");
        assert_eq!(actions[0].intent, ButtonIntent::Destructive);
        let armed = tile_row(
            &alarm,
            &TileState::Alarm {
                condition: ProAlarmCondition::Armed,
            },
            ProTileTemplate::Rectangle,
            0,
            Appearance::Light,
            None,
        );
        assert_eq!(armed.action_style, ProTileActionStyle::Glyphs);
        assert_eq!(armed.actions.row_count(), 2);
        let scene = tile_row(
            &ProDeviceRow {
                kind: ProDeviceKind::Scene,
                access: ProControlAccess::Allowed,
                can_power: true,
                value: "Scene".into(),
                ..Default::default()
            },
            &TileState::Plain,
            ProTileTemplate::Compact,
            1,
            Appearance::Light,
            None,
        );
        assert_eq!(
            scene.control,
            ProTileControl::None,
            "a scene has no power button"
        );
        assert_eq!(scene.value, "", "the card carries no room for a scene");
        assert!(scene.solid_active);
    }
    #[test]
    fn a_tile_narrower_than_a_third_of_its_grid_is_compact() {
        let native = crate::dashboard_layout::NativeLayout::try_from(
            &crate::dashboard_layout::tests::extended_forest(),
        )
        .unwrap();
        let mut node = native
            .nodes
            .get(&NodeId::parse("tile").unwrap())
            .unwrap()
            .clone();
        node.vertical = None;
        node.column_span = Some(Dimension::Count(4));
        assert_eq!(
            tile_template(&node, false, Some(12)),
            ProTileTemplate::Compact
        );
        assert_eq!(
            tile_template(&node, false, None),
            ProTileTemplate::Rectangle,
            "an unknown column count cannot judge a span"
        );
        node.column_span = Some(Dimension::Count(5));
        assert_eq!(
            tile_template(&node, false, Some(12)),
            ProTileTemplate::Rectangle
        );
        assert_eq!(tile_template(&node, true, Some(12)), ProTileTemplate::Large);
        node.row_span = None;
        assert_eq!(
            tile_rows(&node),
            0,
            "no configured rows: the template decides"
        );
        node.row_span = Some(Dimension::Count(1));
        assert_eq!(tile_rows(&node), 1);
    }
}
