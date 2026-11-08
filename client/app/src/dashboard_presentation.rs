//! Product adapter from a validated server forest to injected Pro presentation.
use crate::{
    IconGlyph, ProGestureAccess, ProLayoutColumns, ProLayoutFlow, ProLayoutHeading, ProLayoutInput,
    ProLayoutKind, ProLayoutMetricsPolicy, ProLayoutNode, ProLayoutSpan, ProLayoutSpanRule,
    ProRecipeRow, ProRecipeStyle,
};
use crate::{
    dashboard::{Layout, ProjectionRequest},
    dashboard_layout::{Dimension, HeadingStyle, Kind, Node, RecipeKind},
    device_view::DeviceProjection,
};
use slint::Model;
use std::collections::BTreeMap;

pub(crate) struct BoardNode {
    data: Node,
    children: Vec<usize>,
    device: Option<crate::device_view::presentation::CardRow>,
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
                let tile = tile_row(&device_ui, previous.as_ref().map(|node| &node.tile));
                ProLayoutNode {
                    viewport_min_width: data.viewport_min_width,
                    viewport_max_width: data.viewport_max_width,
                    token: self.token_base() + index as i32,
                    generation,
                    widget: node.widget.ui(
                        data,
                        node.device.as_ref(),
                        previous.as_ref(),
                        (self.token_base() + index as i32, generation),
                        icon,
                        matches!(
                            recipe.tap,
                            ProGestureAccess::Enabled | ProGestureAccess::EnabledCommand
                        ),
                    ),
                    kind,
                    flow,
                    title: if data.kind == Kind::View
                        || (index == 0 && matches!(self.target, BoardTarget::Popup))
                    {
                        ""
                    } else if kind == ProLayoutKind::Diagnostic && data.title.is_empty() {
                        data.source.variant.as_str()
                    } else {
                        data.title.as_str()
                    }
                    .into(),
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
                        HeadingStyle::Default if data.gap.is_some() => ProLayoutHeading::Section,
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
                    gap: data.gap.unwrap_or(12) as f32,
                    metrics_policy: if data.gap.is_some() || data.minimum_column_width.is_some() {
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
                    tile_template: if node.square || data.vertical == Some(true) {
                        crate::ProTileTemplate::Large
                    } else {
                        crate::ProTileTemplate::Rectangle
                    },
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
        | crate::ProDeviceKind::Alarm => true,
        crate::ProDeviceKind::Helper
        | crate::ProDeviceKind::Observation
        | crate::ProDeviceKind::Unsupported => false,
    }
}
fn tile_row(
    device: &crate::ProDeviceRow,
    previous: Option<&crate::ProTileRow>,
) -> crate::ProTileRow {
    use crate::{ProActionPhase as P, ProControlAccess, ProTileControl as C};
    let previous =
        previous.filter(|row| row.token == device.token && row.generation == device.generation);
    let primary = device.values.iter().find(|value| {
        device.has_primary_value
            && value.token == device.primary_value_token
            && value.mode == crate::ProValueMode::Single
    });
    let actions: Vec<_> = device
        .cover_operations
        .iter()
        .map(|row| (row.token, row.label))
        .chain(
            device
                .lock_operations
                .iter()
                .map(|row| (row.token, row.label)),
        )
        .chain(
            device
                .alarm_operations
                .iter()
                .map(|row| (row.token, row.label)),
        )
        .map(|(token, label)| crate::ProTileAction {
            token,
            label,
            ..Default::default()
        })
        .collect();
    let choices: Vec<_> = device
        .choices
        .iter()
        .map(|row| crate::ChoiceRow {
            token: row.token,
            label: row.label,
            selected: row.selected,
            disabled: false,
        })
        .collect();
    let control = if device.summary_only {
        C::None
    } else if device.can_brightness {
        C::Level
    } else if primary.is_some() {
        C::Stepper
    } else if !actions.is_empty() {
        C::Actions
    } else if !choices.is_empty() {
        C::Levels
    } else if device.can_power {
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
    crate::ProTileRow {
        token: device.token,
        generation: device.generation,
        title: device.title.clone(),
        value: format!(
            "{}{}{}",
            device.value,
            if device.unit.is_empty() { "" } else { " " },
            device.unit
        )
        .into(),
        detail: if !device.action_label.is_empty() {
            device.action_label.clone()
        } else if device.access == ProControlAccess::Denied {
            "Read-only".into()
        } else {
            device.quality.clone()
        },
        glyph: device.glyph.clone(),
        active: device.power_known && device.power_on,
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
        // The surface is always a safe route to detail. Buttons retain separate
        // exact capability intents; no transport call is generated by opening.
        tap: crate::ProTileTap::Open,
        hold_opens: true,
        control,
        level: device.brightness_percent,
        level_known: device.brightness_known,
        level_minimum: device.brightness_minimum,
        level_maximum: device.brightness_maximum,
        level_increment: device.brightness_step,
        levels: crate::device_view::presentation::nested(&choices, previous.map(|row| &row.levels)),
        levels_active: choices
            .iter()
            .find(|row| row.selected)
            .or_else(|| choices.first())
            .map_or(-1, |row| row.token),
        actions: crate::device_view::presentation::nested(
            &actions,
            previous.map(|row| &row.actions),
        ),
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
        let tile = tile_row(&device, None);
        assert_eq!(tile.tap, crate::ProTileTap::Open);
        assert_eq!(tile.control, crate::ProTileControl::Level);
        assert_eq!(
            (tile.level_minimum, tile.level_maximum, tile.level_increment),
            (0.1, 0.3, 0.1)
        );
        assert!(!tile.commands_disabled);
        device.access = crate::ProControlAccess::Denied;
        let denied = tile_row(&device, Some(&tile));
        assert!(denied.commands_disabled);
        assert_eq!(
            denied.tap,
            crate::ProTileTap::Open,
            "read-only detail remains navigable"
        );
        assert_eq!(denied.token, 0);
        assert_eq!(denied.generation, 9);
    }
}
