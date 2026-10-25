//! Validated inert layout forest. Server source identities are never row indices.
use crate::dashboard::LayoutFailure;
use buffa::EnumValue;
use home_client_model::EntityId;
use home_wire_contract::home::dashboard::v1 as wire;
use std::collections::{BTreeMap, BTreeSet};

pub(crate) const MAX_NODES: usize = 128 + 256 + 2048;
const MAX_DEPTH: usize = 18; // view + section + card levels 0..16, importer059

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub(crate) struct NodeId(String);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Kind {
    View,
    Section,
    Grid,
    HorizontalStack,
    Heading,
    Tile,
    Unsupported,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Dimension {
    Count(u8),
    Full,
    Auto,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum HeadingStyle {
    Default,
    Title,
    Subtitle,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Source {
    pub file: String,
    pub line: u32,
    pub column: u32,
    pub path: String,
    pub variant: String,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Node {
    pub id: NodeId,
    pub route: Option<crate::dashboard::RoomId>,
    pub kind: Kind,
    pub source: Source,
    pub title: String,
    pub entity: Option<EntityId>,
    pub children: Vec<NodeId>,
    pub diagnostic: String,
    pub columns: Option<u8>,
    pub column_span: Option<Dimension>,
    pub row_span: Option<Dimension>,
    pub square: Option<bool>,
    pub vertical: Option<bool>,
    pub hide_state: Option<bool>,
    pub heading_style: HeadingStyle,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct NativeLayout {
    pub views: Vec<NodeId>,
    pub nodes: BTreeMap<NodeId, Node>,
}
fn bounded(value: &str, empty: bool, maximum: usize) -> bool {
    (empty || !value.is_empty()) && value.len() <= maximum && !value.chars().any(char::is_control)
}
fn identity(value: &str) -> Result<NodeId, LayoutFailure> {
    if bounded(value, false, 4096) {
        Ok(NodeId(value.to_owned()))
    } else {
        Err(LayoutFailure::Invalid)
    }
}
fn dimension(value: Option<&wire::LayoutDimension>) -> Result<Option<Dimension>, LayoutFailure> {
    value
        .map(|value| {
            if !value.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            }
            match value.kind {
                EnumValue::Known(wire::LayoutDimensionKind::Count)
                    if (1..=12).contains(&value.count) =>
                {
                    Ok(Dimension::Count(value.count as u8))
                }
                EnumValue::Known(wire::LayoutDimensionKind::Full) if value.count == 0 => {
                    Ok(Dimension::Full)
                }
                EnumValue::Known(wire::LayoutDimensionKind::Auto) if value.count == 0 => {
                    Ok(Dimension::Auto)
                }
                EnumValue::Known(
                    wire::LayoutDimensionKind::Count
                    | wire::LayoutDimensionKind::Full
                    | wire::LayoutDimensionKind::Auto
                    | wire::LayoutDimensionKind::Unspecified,
                )
                | EnumValue::Unknown(_) => Err(LayoutFailure::Unsupported),
            }
        })
        .transpose()
}
fn kind(value: EnumValue<wire::LayoutKind>) -> Kind {
    match value {
        EnumValue::Known(wire::LayoutKind::View) => Kind::View,
        EnumValue::Known(wire::LayoutKind::Section) => Kind::Section,
        EnumValue::Known(wire::LayoutKind::Grid) => Kind::Grid,
        EnumValue::Known(wire::LayoutKind::HorizontalStack) => Kind::HorizontalStack,
        EnumValue::Known(wire::LayoutKind::Heading) => Kind::Heading,
        EnumValue::Known(wire::LayoutKind::Tile) => Kind::Tile,
        EnumValue::Known(wire::LayoutKind::Unsupported | wire::LayoutKind::Unspecified)
        | EnumValue::Unknown(_) => Kind::Unsupported,
    }
}
impl NativeLayout {
    pub fn root(&self, room: Option<&crate::dashboard::RoomId>) -> Option<&NodeId> {
        match room {
            None => self.views.first(),
            Some(room) => self.views.iter().find(|id| {
                self.nodes
                    .get(*id)
                    .is_some_and(|node| node.route.as_ref() == Some(room))
            }),
        }
    }
    pub fn ordered_nodes(&self, root: &NodeId) -> Vec<&Node> {
        let mut result = Vec::new();
        let mut work = vec![root];
        while let Some(id) = work.pop() {
            if let Some(node) = self.nodes.get(id) {
                result.push(node);
                work.extend(node.children.iter().rev());
            }
        }
        result
    }
}
impl TryFrom<&wire::NativeLayout> for NativeLayout {
    type Error = LayoutFailure;
    fn try_from(value: &wire::NativeLayout) -> Result<Self, Self::Error> {
        if value.nodes.len() > MAX_NODES || value.views.len() > 128 {
            return Err(LayoutFailure::Budget);
        }
        if value.views.is_empty() || value.nodes.is_empty() {
            return Err(LayoutFailure::Invalid);
        }
        let future = value.version != 1 || !value.__buffa_unknown_fields.is_empty();
        let mut nodes = BTreeMap::new();
        let mut child_count = 0usize;
        for node in &value.nodes {
            let id = identity(&node.id)?;
            let source = node.source.as_option().ok_or(LayoutFailure::Invalid)?;
            if !bounded(&source.file, false, 4096)
                || !bounded(&source.path, false, 4096)
                || !bounded(&source.variant, false, 4096)
                || !bounded(&node.title, true, 4096)
                || !bounded(&node.diagnostic, true, 4096)
            {
                return Err(LayoutFailure::Invalid);
            }
            child_count = child_count.saturating_add(node.children.len());
            if child_count > MAX_NODES {
                return Err(LayoutFailure::Budget);
            }
            let mut parsed = Node {
                id: id.clone(),
                route: if node.route.is_empty() {
                    None
                } else {
                    Some(crate::dashboard::RoomId::parse(&node.route)?)
                },
                kind: kind(node.kind),
                source: Source {
                    file: source.file.clone(),
                    line: source.line,
                    column: source.column,
                    path: source.path.clone(),
                    variant: source.variant.clone(),
                },
                title: node.title.clone(),
                entity: None,
                children: node
                    .children
                    .iter()
                    .map(|id| identity(id))
                    .collect::<Result<_, _>>()?,
                diagnostic: node.diagnostic.clone(),
                columns: None,
                column_span: None,
                row_span: None,
                square: node.square,
                vertical: node.vertical,
                hide_state: node.hide_state,
                heading_style: HeadingStyle::Default,
            };
            let columns = if node.columns == 0 {
                Some(None)
            } else if node.columns <= 12 {
                Some(Some(node.columns as u8))
            } else {
                None
            };
            let style = match node.heading_style {
                EnumValue::Known(
                    wire::NativeHeadingStyle::Default | wire::NativeHeadingStyle::Unspecified,
                ) => Some(HeadingStyle::Default),
                EnumValue::Known(wire::NativeHeadingStyle::Title) => Some(HeadingStyle::Title),
                EnumValue::Known(wire::NativeHeadingStyle::Subtitle) => {
                    Some(HeadingStyle::Subtitle)
                }
                EnumValue::Unknown(_) => None,
            };
            match (
                dimension(node.column_span.as_option()),
                dimension(node.row_span.as_option()),
                columns,
                style,
            ) {
                (Ok(column_span), Ok(row_span), Some(columns), Some(style)) => {
                    parsed.column_span = column_span;
                    parsed.row_span = row_span;
                    parsed.columns = columns;
                    parsed.heading_style = style;
                }
                _ => {
                    parsed.kind = Kind::Unsupported;
                }
            }
            if future
                || !node.__buffa_unknown_fields.is_empty()
                || !source.__buffa_unknown_fields.is_empty()
            {
                parsed.kind = Kind::Unsupported;
            }
            if parsed.kind == Kind::Tile {
                parsed.entity = Some(node.entity_id.parse().map_err(|_| LayoutFailure::Invalid)?);
                if !parsed.children.is_empty() {
                    return Err(LayoutFailure::Invalid);
                }
            } else if !node.entity_id.is_empty() && parsed.kind != Kind::Unsupported {
                return Err(LayoutFailure::Invalid);
            }
            if parsed.kind == Kind::Heading && !parsed.children.is_empty() {
                return Err(LayoutFailure::Invalid);
            }
            if parsed.kind == Kind::Unsupported && parsed.diagnostic.is_empty() {
                parsed.diagnostic = if future {
                    "Version du layout non prise en charge."
                } else {
                    "Élément ou propriété non pris en charge."
                }
                .into();
            }
            if nodes.insert(id, parsed).is_some() {
                return Err(LayoutFailure::Invalid);
            }
        }
        let views = value
            .views
            .iter()
            .map(|id| identity(id))
            .collect::<Result<Vec<_>, _>>()?;
        let mut visited = BTreeSet::new();
        let mut work = views
            .iter()
            .rev()
            .map(|id| (id.clone(), None, 0usize, false))
            .collect::<Vec<_>>();
        while let Some((id, parent, depth, blocked)) = work.pop() {
            if depth > MAX_DEPTH {
                return Err(LayoutFailure::Budget);
            }
            if !visited.insert(id.clone()) {
                return Err(LayoutFailure::Invalid);
            } // cycle/shared root/child
            let node = nodes.get_mut(&id).ok_or(LayoutFailure::Invalid)?;
            let valid = match (parent, node.kind) {
                (None, Kind::View | Kind::Unsupported) => true,
                (Some(Kind::View), Kind::Section | Kind::Unsupported) => true,
                (
                    Some(Kind::Section | Kind::Grid | Kind::HorizontalStack),
                    Kind::Grid
                    | Kind::HorizontalStack
                    | Kind::Tile
                    | Kind::Heading
                    | Kind::Unsupported,
                ) => true,
                (
                    Some(Kind::Unsupported),
                    Kind::View
                    | Kind::Section
                    | Kind::Grid
                    | Kind::HorizontalStack
                    | Kind::Tile
                    | Kind::Heading
                    | Kind::Unsupported,
                ) => true,
                (
                    None
                    | Some(
                        Kind::View
                        | Kind::Section
                        | Kind::Grid
                        | Kind::HorizontalStack
                        | Kind::Tile
                        | Kind::Heading,
                    ),
                    Kind::View
                    | Kind::Section
                    | Kind::Grid
                    | Kind::HorizontalStack
                    | Kind::Tile
                    | Kind::Heading
                    | Kind::Unsupported,
                ) => false,
            };
            if !valid {
                return Err(LayoutFailure::Invalid);
            }
            if blocked {
                node.kind = Kind::Unsupported;
                node.entity = None;
                node.diagnostic = "Élément inactif : parent non pris en charge.".into();
            }
            for child in node.children.iter().rev() {
                work.push((
                    child.clone(),
                    Some(node.kind),
                    depth + 1,
                    blocked || node.kind == Kind::Unsupported,
                ));
            }
        }
        if visited.len() != nodes.len() {
            return Err(LayoutFailure::Invalid);
        }
        Ok(Self { views, nodes })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn node(id: &str, kind: wire::LayoutKind, children: &[&str]) -> wire::LayoutNode {
        wire::LayoutNode {
            id: id.into(),
            kind: kind.into(),
            source: wire::LayoutSource {
                file: "synthetic.yaml".into(),
                path: id.into(),
                variant: "synthetic".into(),
                line: 1,
                ..Default::default()
            }
            .into(),
            children: children.iter().map(|id| (*id).into()).collect(),
            entity_id: if kind == wire::LayoutKind::Tile {
                "synthetic-entity".into()
            } else {
                String::new()
            },
            ..Default::default()
        }
    }
    fn forest() -> wire::NativeLayout {
        wire::NativeLayout {
            version: 1,
            views: vec!["view".into()],
            nodes: vec![
                node("view", wire::LayoutKind::View, &["section"]),
                node("section", wire::LayoutKind::Section, &["grid"]),
                node("grid", wire::LayoutKind::Grid, &["tile", "unknown"]),
                node("tile", wire::LayoutKind::Tile, &[]),
                node("unknown", wire::LayoutKind::Unsupported, &[]),
            ],
            ..Default::default()
        }
    }
    #[test]
    fn malformed_forests_never_turn_cycles_shared_or_orphaned_data_into_widgets() {
        let mut cases = vec![forest(); 5];
        cases[0].nodes[2].children.push("view".into());
        cases[1].nodes[1].children.push("tile".into());
        cases[2]
            .nodes
            .push(node("orphan", wire::LayoutKind::Tile, &[]));
        cases[3].nodes[2].children.push("missing".into());
        cases[4].views.push("view".into());
        for wire in cases {
            assert_eq!(NativeLayout::try_from(&wire), Err(LayoutFailure::Invalid));
        }
        assert!(NativeLayout::try_from(&forest()).is_ok());
    }
    #[test]
    fn future_version_and_parent_variants_are_visible_sourced_and_inert() {
        let mut wire = forest();
        wire.nodes[2].kind = EnumValue::Unknown(99);
        let parsed = NativeLayout::try_from(&wire).unwrap();
        let tile = &parsed.nodes[&identity("tile").unwrap()];
        assert_eq!(tile.kind, Kind::Unsupported);
        assert!(tile.entity.is_none() && !tile.diagnostic.is_empty());
        assert_eq!(tile.source.file, "synthetic.yaml");
        wire = forest();
        wire.version = 99;
        let parsed = NativeLayout::try_from(&wire).unwrap();
        assert!(
            parsed
                .nodes
                .values()
                .all(|node| node.kind == Kind::Unsupported
                    && node.entity.is_none()
                    && !node.diagnostic.is_empty())
        );
    }
    #[test]
    fn parser_accepts_the_full_importer_card_depth_and_rejects_more() {
        let mut wire = wire::NativeLayout {
            version: 1,
            views: vec!["view".into()],
            nodes: vec![
                node("view", wire::LayoutKind::View, &["section"]),
                node("section", wire::LayoutKind::Section, &["card0"]),
            ],
            ..Default::default()
        };
        for level in 0..=16 {
            let id = format!("card{level}");
            let next = format!("card{}", level + 1);
            let next_ref = next.as_str();
            wire.nodes.push(node(
                &id,
                if level == 16 {
                    wire::LayoutKind::Tile
                } else {
                    wire::LayoutKind::Grid
                },
                if level == 16 {
                    &[]
                } else {
                    std::slice::from_ref(&next_ref)
                },
            ));
        }
        assert!(NativeLayout::try_from(&wire).is_ok());
        let last = wire.nodes.last_mut().unwrap();
        last.kind = wire::LayoutKind::Grid.into();
        last.entity_id.clear();
        last.children.push("overflow".into());
        wire.nodes
            .push(node("overflow", wire::LayoutKind::Tile, &[]));
        assert_eq!(NativeLayout::try_from(&wire), Err(LayoutFailure::Budget));
    }
}
