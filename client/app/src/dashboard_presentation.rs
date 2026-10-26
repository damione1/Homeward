//! Product adapter from a validated server forest to injected Pro presentation.
use crate::{
    ProGestureAccess, ProLayoutCell, ProLayoutColumns, ProLayoutFlow, ProLayoutHeading,
    ProLayoutInput, ProLayoutKind, ProLayoutNode, ProLayoutSpan, ProRecipeRow, ProRecipeStyle,
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
    pub fn target(&self) -> BoardTarget {
        self.target
    }
    pub fn rows(&self, previous: &slint::VecModel<ProLayoutNode>) -> Vec<ProLayoutNode> {
        self.nodes
            .iter()
            .enumerate()
            .map(|(index, node)| {
                let previous = previous.row_data(index);
                let data = &node.data;
                let kind = match data.kind {
                    Kind::View
                    | Kind::Section
                    | Kind::Grid
                    | Kind::HorizontalStack
                    | Kind::VerticalStack
                    | Kind::Popup
                    | Kind::ChipGroup => ProLayoutKind::Container,
                    Kind::Heading => ProLayoutKind::Heading,
                    Kind::Tile => {
                        if node.device.is_some() {
                            ProLayoutKind::Device
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
                    | Kind::Unsupported => ProLayoutFlow::Vertical,
                };
                let children = node
                    .children
                    .iter()
                    .map(|index| {
                        let child = &self.nodes[*index].data;
                        let (span, count) = match child.column_span {
                            None | Some(Dimension::Auto) => (ProLayoutSpan::Automatic, 0),
                            Some(Dimension::Full) => (ProLayoutSpan::Full, 0),
                            Some(Dimension::Count(count)) => {
                                (ProLayoutSpan::Count, i32::from(count))
                            }
                        };
                        ProLayoutInput {
                            index: *index as i32,
                            column_span: span,
                            columns: count,
                            rows: match child.row_span {
                                Some(Dimension::Count(rows)) => i32::from(rows),
                                None | Some(Dimension::Auto | Dimension::Full) => 1,
                            },
                        }
                    })
                    .collect::<Vec<_>>();
                let mut device = node.device.clone().unwrap_or_default();
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
                        "Une interaction configurée est indisponible.".into()
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
                ProLayoutNode {
                    kind,
                    flow,
                    title: if data.kind == Kind::View {
                        ""
                    } else if kind == ProLayoutKind::Diagnostic && data.title.is_empty() {
                        data.source.variant.as_str()
                    } else {
                        data.title.as_str()
                    }
                    .into(),
                    description: if kind == ProLayoutKind::Diagnostic
                        && data.entity.is_some()
                        && node.device.is_none()
                    {
                        "Aucune observation autorisée pour cet élément."
                    } else {
                        data.diagnostic.as_str()
                    }
                    .into(),
                    source: source.into(),
                    heading: match data.heading_style {
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
                        | Kind::Unsupported => ProLayoutColumns::Fixed,
                    },
                    span_base: if data.kind == Kind::Section { 12 } else { 0 },
                    square: node.square,
                    vertical: data.vertical.unwrap_or(false),
                    hide_state: data.hide_state.unwrap_or(false),
                    children,
                    device: device.ui(previous.as_ref().map(|node| &node.device)),
                    recipe,
                    ..Default::default()
                }
            })
            .collect()
    }
}
pub(crate) fn bind(window: &crate::HomewardWindow) {
    slint_kit_pro::bind_layout_arranger!(window, on_layout_arrange, ProLayoutSpan, ProLayoutCell);
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dashboard_layout::NodeId;

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
            entities: vec!["synthetic-entity".parse().unwrap()],
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
        let first_board = Board::project(Some(&layout), &request, &devices);
        let first = first_board.rows(&slint::VecModel::default());
        let previous = slint::VecModel::from(first.clone());
        devices.rows[0].value = "New observation".into();
        devices
            .native_devices
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap()
            .value = "New observation".into();
        let next = Board::project(Some(&layout), &request, &devices).rows(&previous);
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
        let changed = Board::project(Some(&layout), &request, &devices).rows(&previous);
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
        let board = Board::project(Some(&layout), &ProjectionRequest::default(), &devices);
        let rows = board.rows(&slint::VecModel::default());
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
}
