//! Product adapter from a validated server forest to injected Pro presentation.
use crate::{
    ProLayoutCell, ProLayoutColumns, ProLayoutFlow, ProLayoutHeading, ProLayoutInput,
    ProLayoutKind, ProLayoutNode, ProLayoutSpan,
};
use crate::{
    dashboard::{Layout, ProjectionRequest},
    dashboard_layout::{Dimension, HeadingStyle, Kind, Node},
    device_view::DeviceProjection,
};
use slint::{Model, ModelRc};
use std::collections::BTreeMap;

pub(crate) struct BoardNode {
    data: Node,
    children: Vec<usize>,
    device: Option<crate::device_view::presentation::CardRow>,
    square: bool,
}
#[derive(Clone, Copy, Default)]
pub(crate) enum BoardTarget {
    #[default]
    Clear,
    Retain,
    Home,
    Room,
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
        let Some(native) = layout.and_then(|layout| layout.native.as_ref()) else {
            return Self::default();
        };
        if request.detail.is_some() || !request.query.is_empty() {
            return Self {
                target: BoardTarget::Retain,
                nodes: Vec::new(),
            };
        }
        let target = if request.room.is_some() {
            BoardTarget::Room
        } else {
            BoardTarget::Home
        };
        let Some(root) = native.root(request.room.as_ref()) else {
            return Self {
                target,
                nodes: Vec::new(),
            };
        };
        let nodes = native.ordered_nodes(root);
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
                    let device = node
                        .entity
                        .as_ref()
                        .and_then(|id| devices.entities.iter().position(|entity| entity == id))
                        .and_then(|index| devices.rows.get(index))
                        .cloned();
                    BoardNode {
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
                let data = &node.data;
                let kind = match data.kind {
                    Kind::View | Kind::Section | Kind::Grid | Kind::HorizontalStack => {
                        ProLayoutKind::Container
                    }
                    Kind::Heading => ProLayoutKind::Heading,
                    Kind::Tile => {
                        if node.device.is_some() {
                            ProLayoutKind::Device
                        } else {
                            ProLayoutKind::Diagnostic
                        }
                    }
                    Kind::Unsupported => ProLayoutKind::Diagnostic,
                };
                let flow = match data.kind {
                    Kind::View | Kind::Section | Kind::Grid => ProLayoutFlow::Grid,
                    Kind::HorizontalStack => ProLayoutFlow::Horizontal,
                    Kind::Heading | Kind::Tile | Kind::Unsupported => ProLayoutFlow::Vertical,
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
                    description: if data.kind == Kind::Tile && node.device.is_none() {
                        "Aucune observation autorisée pour cet élément."
                    } else {
                        data.diagnostic.as_str()
                    }
                    .into(),
                    source: format!(
                        "{}:{}:{} · {} · {}",
                        data.source.file,
                        data.source.line,
                        data.source.column,
                        data.source.path,
                        data.source.variant
                    )
                    .into(),
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
                        | Kind::Heading
                        | Kind::Tile
                        | Kind::Unsupported => ProLayoutColumns::Fixed,
                    },
                    span_base: if data.kind == Kind::Section { 12 } else { 0 },
                    square: node.square,
                    vertical: data.vertical.unwrap_or(false),
                    hide_state: data.hide_state.unwrap_or(false),
                    children: ModelRc::from(children.as_slice()),
                    device: device.ui(previous.row_data(index).as_ref().map(|node| &node.device)),
                    ..Default::default()
                }
            })
            .collect()
    }
}
pub(crate) fn bind(window: &crate::HomewardWindow) {
    slint_kit_pro::bind_layout_arranger!(window, on_layout_arrange, ProLayoutSpan, ProLayoutCell);
}
