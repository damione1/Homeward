//! Validated layout forest. Server source identities are never row indices.
use crate::dashboard::LayoutFailure;
use buffa::EnumValue;
use home_client_model::api::{CapabilityName, EntityName};
use home_client_model::dashboard::Recipes;
pub(crate) use home_client_model::dashboard::{NodeId, Source};
use home_wire_contract::dashboard_contract::{decode_interactions, decode_predicate};
use home_wire_contract::home::api::v2 as wire;
use std::collections::{BTreeMap, BTreeSet};

pub(crate) const MAX_NODES: usize = 128 + 256 + 2048;
const MAX_DEPTH: usize = 18; // view + section + card levels 0..16, importer059

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Kind {
    View,
    Section,
    Grid,
    HorizontalStack,
    VerticalStack,
    Recipe,
    Popup,
    ChipGroup,
    Heading,
    Tile,
    Widget,
    Unsupported,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum RecipeKind {
    Template,
    Select,
    Alarm,
    BubbleButton,
    BubblePopup,
    Chips,
    TemplateChip,
    Button,
    Entity,
    Text,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub(crate) enum FeatureKind {
    Toggle,
    Brightness,
    ColorTemperature,
    TargetTemperature,
    ClimateModes,
    FanSpeed,
    NumericInput,
    CoverOpenClose,
    CoverPosition,
    AlarmModes,
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
pub(crate) struct Node {
    pub id: NodeId,
    pub route: Option<crate::dashboard::RoomId>,
    pub kind: Kind,
    pub icon: wire::DashboardIcon,
    pub source: Source,
    pub title: String,
    pub secondary: String,
    pub recipe_kind: Option<RecipeKind>,
    pub recipes: Option<Recipes>,
    pub features: Vec<FeatureKind>,
    pub summary_only: bool,
    pub entity: Option<EntityName>,
    pub capability: Option<CapabilityName>,
    pub children: Vec<NodeId>,
    pub diagnostic: String,
    pub columns: Option<u8>,
    pub column_span: Option<Dimension>,
    pub row_span: Option<Dimension>,
    pub square: Option<bool>,
    pub vertical: Option<bool>,
    pub hide_state: Option<bool>,
    pub heading_style: HeadingStyle,
    pub widget: Option<crate::dashboard_widgets::Widget>,
    pub minimum_column_width: Option<u32>,
    pub gap: Option<u32>,
    pub responsive_spans: Vec<(u8, u8)>,
    pub viewport_min_width: i32,
    pub viewport_max_width: i32,
}
impl Node {
    pub fn device_card(&self) -> bool {
        match self.kind {
            Kind::Tile => true,
            Kind::Widget => self.widget.as_ref().is_some_and(|widget| {
                matches!(
                    widget.content,
                    crate::dashboard_widgets::Content::Thermostat { .. }
                )
            }),
            Kind::Recipe => match self.recipe_kind {
                Some(RecipeKind::Select | RecipeKind::Alarm | RecipeKind::Entity) => true,
                Some(
                    RecipeKind::Template
                    | RecipeKind::BubbleButton
                    | RecipeKind::BubblePopup
                    | RecipeKind::Chips
                    | RecipeKind::TemplateChip
                    | RecipeKind::Button
                    | RecipeKind::Text,
                )
                | None => false,
            },
            Kind::View
            | Kind::Section
            | Kind::Grid
            | Kind::HorizontalStack
            | Kind::VerticalStack
            | Kind::Heading
            | Kind::Popup
            | Kind::ChipGroup
            | Kind::Unsupported => false,
        }
    }
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
    NodeId::parse(value).map_err(|_| LayoutFailure::Invalid)
}
fn dimension(
    value: Option<&wire::DashboardLayoutDimension>,
) -> Result<Option<Dimension>, LayoutFailure> {
    value
        .map(|v| {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            match v.size.as_ref() {
                Some(wire::dashboard_layout_dimension::Size::Count(v)) if (1..=12).contains(v) => {
                    Ok(Dimension::Count(
                        u8::try_from(*v).map_err(|_| LayoutFailure::Invalid)?,
                    ))
                }
                Some(wire::dashboard_layout_dimension::Size::Full(v))
                    if v.__buffa_unknown_fields.is_empty() =>
                {
                    Ok(Dimension::Full)
                }
                Some(wire::dashboard_layout_dimension::Size::Auto(v))
                    if v.__buffa_unknown_fields.is_empty() =>
                {
                    Ok(Dimension::Auto)
                }
                _ => Err(LayoutFailure::Unsupported),
            }
        })
        .transpose()
}
fn recipe_kind(value: EnumValue<wire::DashboardRecipeKind>) -> Result<Option<RecipeKind>, ()> {
    Ok(match value {
        EnumValue::Known(wire::DashboardRecipeKind::Unspecified) => None,
        EnumValue::Known(wire::DashboardRecipeKind::Template) => Some(RecipeKind::Template),
        EnumValue::Known(wire::DashboardRecipeKind::Select) => Some(RecipeKind::Select),
        EnumValue::Known(wire::DashboardRecipeKind::Alarm) => Some(RecipeKind::Alarm),
        EnumValue::Known(wire::DashboardRecipeKind::BubbleButton) => Some(RecipeKind::BubbleButton),
        EnumValue::Known(wire::DashboardRecipeKind::BubblePopup) => Some(RecipeKind::BubblePopup),
        EnumValue::Known(wire::DashboardRecipeKind::Chips) => Some(RecipeKind::Chips),
        EnumValue::Known(wire::DashboardRecipeKind::TemplateChip) => Some(RecipeKind::TemplateChip),
        EnumValue::Known(wire::DashboardRecipeKind::Button) => Some(RecipeKind::Button),
        EnumValue::Known(wire::DashboardRecipeKind::Entity) => Some(RecipeKind::Entity),
        EnumValue::Known(wire::DashboardRecipeKind::Text) => Some(RecipeKind::Text),
        EnumValue::Unknown(_) => return Err(()),
    })
}
fn feature_kind(value: EnumValue<wire::DashboardFeatureKind>) -> Option<FeatureKind> {
    match value {
        EnumValue::Known(wire::DashboardFeatureKind::Toggle) => Some(FeatureKind::Toggle),
        EnumValue::Known(wire::DashboardFeatureKind::Brightness) => Some(FeatureKind::Brightness),
        EnumValue::Known(wire::DashboardFeatureKind::ColorTemperature) => {
            Some(FeatureKind::ColorTemperature)
        }
        EnumValue::Known(wire::DashboardFeatureKind::TargetTemperature) => {
            Some(FeatureKind::TargetTemperature)
        }
        EnumValue::Known(wire::DashboardFeatureKind::ClimateModes) => {
            Some(FeatureKind::ClimateModes)
        }
        EnumValue::Known(wire::DashboardFeatureKind::FanSpeed) => Some(FeatureKind::FanSpeed),
        EnumValue::Known(wire::DashboardFeatureKind::NumericInput) => {
            Some(FeatureKind::NumericInput)
        }
        EnumValue::Known(wire::DashboardFeatureKind::CoverOpenClose) => {
            Some(FeatureKind::CoverOpenClose)
        }
        EnumValue::Known(wire::DashboardFeatureKind::CoverPosition) => {
            Some(FeatureKind::CoverPosition)
        }
        EnumValue::Known(wire::DashboardFeatureKind::AlarmModes) => Some(FeatureKind::AlarmModes),
        EnumValue::Known(wire::DashboardFeatureKind::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
impl Kind {
    fn card(self) -> bool {
        match self {
            Self::Grid
            | Self::HorizontalStack
            | Self::VerticalStack
            | Self::Heading
            | Self::Tile
            | Self::Widget
            | Self::Recipe
            | Self::Popup
            | Self::ChipGroup
            | Self::Unsupported => true,
            Self::View | Self::Section => false,
        }
    }
    fn accepts(self, child: Self) -> bool {
        match self {
            Self::View => child == Self::Section || child == Self::Unsupported,
            Self::Section
            | Self::Grid
            | Self::HorizontalStack
            | Self::VerticalStack
            | Self::Popup => child.card(),
            Self::ChipGroup => child == Self::Recipe || child == Self::Unsupported,
            Self::Unsupported => true,
            Self::Heading | Self::Tile | Self::Recipe | Self::Widget => false,
        }
    }
}
impl Node {
    pub(crate) fn inert(&mut self, diagnostic: &str) {
        self.kind = Kind::Unsupported;
        self.entity = None;
        self.capability = None;
        self.recipes = None;
        self.features.clear();
        self.viewport_min_width = 0;
        self.viewport_max_width = 0;
        self.recipe_kind = None;
        self.widget = None;
        if self.diagnostic.is_empty() {
            self.diagnostic = diagnostic.into();
        }
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
                // Popup bodies belong to their explicitly selected source root;
                // merely loading the home view must not expose them inline.
                if node.kind == Kind::Popup && id != root {
                    continue;
                }
                result.push(node);
                work.extend(node.children.iter().rev());
            }
        }
        result
    }
}
impl TryFrom<&wire::DashboardLayout> for NativeLayout {
    type Error = LayoutFailure;
    fn try_from(value: &wire::DashboardLayout) -> Result<Self, Self::Error> {
        if value.nodes.len() > MAX_NODES || value.views.len() > 128 {
            return Err(LayoutFailure::Budget);
        };
        if !value.__buffa_unknown_fields.is_empty() {
            return Err(LayoutFailure::Unsupported);
        };
        let mut nodes = BTreeMap::new();
        let mut widget_bindings = 0usize;
        let mut resource_widgets = 0usize;
        for raw in &value.nodes {
            let parsed = parse_node(raw)?;
            if let Some(widget) = &parsed.widget {
                widget_bindings = widget_bindings.saturating_add(widget.binding_count());
                if matches!(
                    widget.content,
                    crate::dashboard_widgets::Content::Agenda { .. }
                        | crate::dashboard_widgets::Content::Tasks { .. }
                ) {
                    resource_widgets += 1
                };
                if widget_bindings > 4096 || resource_widgets > 32 {
                    return Err(LayoutFailure::Budget);
                }
            };
            if nodes.insert(parsed.id.clone(), parsed).is_some() {
                return Err(LayoutFailure::Invalid);
            };
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
            .map(|id| (id.clone(), None::<Kind>, 0usize, false))
            .collect::<Vec<_>>();
        while let Some((id, parent, depth, blocked)) = work.pop() {
            if depth > MAX_DEPTH {
                return Err(LayoutFailure::Budget);
            }
            if !visited.insert(id.clone()) {
                return Err(LayoutFailure::Invalid);
            } // cycle/shared root/child
            let node = nodes.get_mut(&id).ok_or(LayoutFailure::Invalid)?;
            let valid = match parent {
                None => matches!(node.kind, Kind::View | Kind::Unsupported),
                Some(parent) => parent.accepts(node.kind),
            };
            if !valid {
                return Err(LayoutFailure::Invalid);
            }
            if blocked {
                node.inert("Item inactive: unsupported parent.");
                node.diagnostic = "Item inactive: unsupported parent.".into();
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

fn parse_node(raw: &wire::DashboardLayoutNode) -> Result<Node, LayoutFailure> {
    use wire::dashboard_layout_node::Content as C;
    if !raw.__buffa_unknown_fields.is_empty() {
        return Err(LayoutFailure::Unsupported);
    };
    let source = raw.source.as_option().ok_or(LayoutFailure::Invalid)?;
    if !source.__buffa_unknown_fields.is_empty()
        || !bounded(&source.file, false, 4096)
        || !bounded(&source.path, false, 4096)
        || !bounded(&source.variant, true, 4096)
        || raw.display_name.len() > 4096
        || raw.display_name.contains('\0')
    {
        return Err(LayoutFailure::Invalid);
    };
    let icon = match raw.icon {
        EnumValue::Known(v) => v,
        _ => return Err(LayoutFailure::Unsupported),
    };
    let mut node = Node {
        id: identity(&raw.id)?,
        route: None,
        kind: Kind::Unsupported,
        icon,
        source: Source {
            file: source.file.clone(),
            line: source.line,
            column: source.column,
            path: source.path.clone(),
            variant: source.variant.clone(),
        },
        title: raw.display_name.clone(),
        secondary: String::new(),
        recipe_kind: None,
        recipes: None,
        features: Vec::new(),
        summary_only: false,
        entity: None,
        capability: None,
        children: Vec::new(),
        diagnostic: String::new(),
        columns: None,
        column_span: None,
        row_span: None,
        square: None,
        vertical: None,
        hide_state: None,
        heading_style: HeadingStyle::Default,
        widget: None,
        minimum_column_width: None,
        gap: None,
        responsive_spans: Vec::new(),
        viewport_min_width: 0,
        viewport_max_width: 0,
    };
    if let Some(placement) = raw.placement.as_option() {
        if !placement.__buffa_unknown_fields.is_empty() {
            return Err(LayoutFailure::Unsupported);
        };
        node.column_span = dimension(placement.column_span.as_option())?;
        node.row_span = dimension(placement.row_span.as_option())?;
        if placement.responsive_spans.len() > 12 {
            return Err(LayoutFailure::Budget);
        };
        let mut seen = BTreeSet::new();
        for span in &placement.responsive_spans {
            if !span.__buffa_unknown_fields.is_empty()
                || !(1..=12).contains(&span.columns)
                || !(1..=span.columns).contains(&span.span)
                || !seen.insert(span.columns)
            {
                return Err(LayoutFailure::Invalid);
            };
            node.responsive_spans.push((
                u8::try_from(span.columns).map_err(|_| LayoutFailure::Invalid)?,
                u8::try_from(span.span).map_err(|_| LayoutFailure::Invalid)?,
            ))
        }
    }
    if let Some(screen) = raw.screen_visibility.as_option() {
        if !screen.__buffa_unknown_fields.is_empty()
            || !(0..=65535).contains(&screen.min_width)
            || !(0..=65535).contains(&screen.max_width)
            || (screen.max_width != 0 && screen.min_width > screen.max_width)
        {
            return Err(LayoutFailure::Invalid);
        };
        node.viewport_min_width = screen.min_width;
        node.viewport_max_width = screen.max_width
    }
    let mut interactions = None;
    let mut children = None;
    let mut width = 0;
    match raw.content.as_ref().ok_or(LayoutFailure::Unsupported)? {
        C::View(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = Kind::View;
            node.route = if v.route.is_empty() {
                None
            } else {
                Some(crate::dashboard::RoomId::parse(&v.route)?)
            };
            children = v.children.as_option();
            node.columns = columns(v.maximum_columns)?;
            width = v.minimum_column_width
        }
        C::Section(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = Kind::Section;
            children = v.children.as_option();
            width = v.minimum_column_width
        }
        C::Grid(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = Kind::Grid;
            children = v.children.as_option();
            node.columns = columns(v.columns)?;
            width = v.minimum_column_width;
            node.square = v.square;
            interactions = v.interactions.as_option()
        }
        C::HorizontalStack(v) | C::VerticalStack(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = if matches!(raw.content.as_ref(), Some(C::HorizontalStack(_))) {
                Kind::HorizontalStack
            } else {
                Kind::VerticalStack
            };
            children = v.children.as_option();
            interactions = v.interactions.as_option()
        }
        C::Heading(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = Kind::Heading;
            node.heading_style = match v.style {
                EnumValue::Known(wire::DashboardHeadingStyle::Default) => HeadingStyle::Default,
                EnumValue::Known(wire::DashboardHeadingStyle::Title) => HeadingStyle::Title,
                EnumValue::Known(wire::DashboardHeadingStyle::Subtitle) => HeadingStyle::Subtitle,
                _ => return Err(LayoutFailure::Unsupported),
            };
            interactions = v.interactions.as_option()
        }
        C::Tile(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = Kind::Tile;
            bind_node(&mut node, &v.capability)?;
            node.features = features(&v.features)?;
            node.summary_only = presentation(v.presentation)?;
            node.vertical = v.vertical;
            node.hide_state = v.hide_observation;
            interactions = v.interactions.as_option()
        }
        C::Recipe(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = Kind::Recipe;
            node.recipe_kind = recipe_kind(v.kind).map_err(|_| LayoutFailure::Unsupported)?;
            if node.recipe_kind.is_none() {
                return Err(LayoutFailure::Unsupported);
            };
            if !v.capability.is_empty() {
                bind_node(&mut node, &v.capability)?
            };
            node.features = features(&v.features)?;
            node.summary_only = presentation(v.presentation)?;
            if v.secondary.len() > 512 || v.secondary.contains('\0') {
                return Err(LayoutFailure::Invalid);
            };
            node.secondary = v.secondary.clone();
            interactions = v.interactions.as_option()
        }
        C::Popup(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = Kind::Popup;
            children = v.children.as_option();
            interactions = v.interactions.as_option()
        }
        C::ChipGroup(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = Kind::ChipGroup;
            children = v.children.as_option();
            interactions = v.interactions.as_option()
        }
        C::Widget(v) => {
            node.kind = Kind::Widget;
            let widget = crate::dashboard_widgets::Widget::try_from(v.as_ref())?;
            if let crate::dashboard_widgets::Content::Thermostat { entity, target, .. } =
                &widget.content
            {
                node.entity = Some(entity.clone());
                node.capability = Some(target.capability.clone())
            };
            node.widget = Some(widget);
            interactions = v.interactions.as_option()
        }
        C::Unsupported(v) => {
            if !v.__buffa_unknown_fields.is_empty() || !bounded(&v.diagnostic, true, 4096) {
                return Err(LayoutFailure::Unsupported);
            };
            node.diagnostic = v.diagnostic.clone();
            node.children = v
                .children
                .iter()
                .map(|v| identity(v))
                .collect::<Result<_, _>>()?;
            if raw.visibility.as_option().is_some() {
                return Err(LayoutFailure::Invalid);
            }
        }
    }
    if !(0..=4096).contains(&width) {
        return Err(LayoutFailure::Budget);
    };
    node.minimum_column_width = if width == 0 {
        None
    } else {
        Some(u32::try_from(width).map_err(|_| LayoutFailure::Invalid)?)
    };
    if let Some(children) = children {
        if !children.__buffa_unknown_fields.is_empty()
            || !(0..=256).contains(&children.gap)
            || children.children.len() > MAX_NODES
        {
            return Err(LayoutFailure::Budget);
        };
        node.children = children
            .children
            .iter()
            .map(|v| identity(v))
            .collect::<Result<_, _>>()?;
        node.gap = Some(u32::try_from(children.gap).map_err(|_| LayoutFailure::Invalid)?)
    }
    let visibility = raw
        .visibility
        .as_option()
        .map(decode_predicate)
        .transpose()
        .map_err(|_| LayoutFailure::Unsupported)?;
    if interactions.is_some() || visibility.is_some() {
        node.recipes = Some(Recipes {
            interactions: interactions
                .map(decode_interactions)
                .transpose()
                .map_err(|_| LayoutFailure::Unsupported)?
                .unwrap_or_default(),
            visibility,
        })
    };
    if let Some(destination) = node.widget.as_ref().and_then(|w| w.destination()).cloned() {
        use home_client_model::dashboard::{Gesture, Intention, Interaction};
        let interaction = Interaction {
            intention: Intention::Navigate(destination),
            confirmation: None,
            source: node.source.clone(),
        };
        let recipes = node.recipes.get_or_insert_with(|| Recipes {
            interactions: BTreeMap::new(),
            visibility: None,
        });
        if recipes
            .interactions
            .get(&Gesture::Tap)
            .is_some_and(|v| v.intention != interaction.intention || v.confirmation.is_some())
        {
            return Err(LayoutFailure::Invalid);
        };
        recipes
            .interactions
            .entry(Gesture::Tap)
            .or_insert(interaction);
    }
    Ok(node)
}
fn columns(value: i32) -> Result<Option<u8>, LayoutFailure> {
    if !(0..=12).contains(&value) {
        Err(LayoutFailure::Budget)
    } else {
        Ok(if value == 0 {
            None
        } else {
            Some(u8::try_from(value).map_err(|_| LayoutFailure::Invalid)?)
        })
    }
}
fn bind_node(node: &mut Node, value: &str) -> Result<(), LayoutFailure> {
    let capability = CapabilityName::try_from(value).map_err(|_| LayoutFailure::Invalid)?;
    node.entity = Some(capability.entity());
    node.capability = Some(capability);
    Ok(())
}
fn presentation(
    value: EnumValue<wire::DashboardControlPresentation>,
) -> Result<bool, LayoutFailure> {
    match value {
        EnumValue::Known(wire::DashboardControlPresentation::Controls) => Ok(false),
        EnumValue::Known(wire::DashboardControlPresentation::Summary) => Ok(true),
        _ => Err(LayoutFailure::Unsupported),
    }
}
fn features(
    values: &[EnumValue<wire::DashboardFeatureKind>],
) -> Result<Vec<FeatureKind>, LayoutFailure> {
    if values.len() > 16 {
        return Err(LayoutFailure::Budget);
    };
    let mut result = Vec::new();
    for value in values {
        let feature = feature_kind(*value).ok_or(LayoutFailure::Unsupported)?;
        if result.contains(&feature) {
            return Err(LayoutFailure::Invalid);
        };
        result.push(feature)
    }
    Ok(result)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use home_client_model::dashboard::{Gesture, Intention};
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
    pub(crate) fn extended_forest() -> wire::NativeLayout {
        let mut wire = forest();
        wire.version = 2;
        wire.nodes[2]
            .children
            .extend(["recipe", "popup", "chips", "vertical"].map(String::from));
        let mut recipe = node("recipe", wire::LayoutKind::Recipe, &[]);
        recipe.recipe_kind = wire::NativeRecipeKind::Template.into();
        recipe.title = "Plain text\n<script>not executed</script>".into();
        recipe.secondary = "Server-projected text".into();
        recipe.recipes = wire::NativeRecipes {
            version: 1,
            interactions: vec![wire::NativeInteraction {
                gesture: wire::NativeGesture::Tap.into(),
                intention: Some(wire::native_interaction::Intention::MoreInfo(
                    wire::NativeEntityTarget {
                        entity_id: "synthetic-entity".into(),
                        ..Default::default()
                    }
                    .into(),
                )),
                confirmation: wire::NativeConfirmation::default().into(),
                source: recipe.source.clone(),
                ..Default::default()
            }],
            ..Default::default()
        }
        .into();
        let mut popup = node("popup", wire::LayoutKind::Popup, &["popup-tile"]);
        popup.recipe_kind = wire::NativeRecipeKind::BubblePopup.into();
        let mut chips = node("chips", wire::LayoutKind::ChipGroup, &["chip"]);
        chips.recipe_kind = wire::NativeRecipeKind::Chips.into();
        let mut chip = node("chip", wire::LayoutKind::Recipe, &[]);
        chip.recipe_kind = wire::NativeRecipeKind::TemplateChip.into();
        wire.nodes.extend([
            recipe,
            popup,
            chips,
            chip,
            node("popup-tile", wire::LayoutKind::Tile, &[]),
            node("vertical", wire::LayoutKind::VerticalStack, &["text"]),
        ]);
        let mut text = node("text", wire::LayoutKind::Recipe, &[]);
        text.recipe_kind = wire::NativeRecipeKind::Text.into();
        text.title = "Bounded plain text".into();
        wire.nodes.push(text);
        wire
    }
    fn get<'a>(layout: &'a NativeLayout, id: &str) -> &'a Node {
        &layout.nodes[&identity(id).unwrap()]
    }
    fn assert_inert(node: &Node) {
        assert_eq!(node.kind, Kind::Unsupported);
        assert!(node.entity.is_none() && node.recipes.is_none());
        assert!(node.features.is_empty() && node.recipe_kind.is_none());
        assert!(!node.diagnostic.is_empty());
        assert_eq!(node.source.file, "synthetic.yaml");
    }
    #[test]
    fn screen_bounds_are_presentation_data_and_invalid_or_legacy_bounds_are_inert() {
        use buffa::Message;
        let mut wire = forest();
        wire.version = 3;
        wire.nodes[3].screen_visibility = wire::NativeScreenVisibility {
            min_width: 768,
            max_width: 1199,
            ..Default::default()
        }
        .into();
        let parsed = NativeLayout::try_from(&wire).unwrap();
        let tile = get(&parsed, "tile");
        assert_eq!(
            (tile.viewport_min_width, tile.viewport_max_width),
            (768, 1199)
        );
        assert_eq!(tile.kind, Kind::Tile);
        assert!(tile.entity.is_some());
        for condition in [
            wire::NativeScreenVisibility {
                min_width: 1200,
                max_width: 767,
                ..Default::default()
            },
            wire::NativeScreenVisibility {
                min_width: 65536,
                ..Default::default()
            },
            wire::NativeScreenVisibility::decode_from_slice(&[24, 1]).unwrap(),
        ] {
            wire.nodes[3].screen_visibility = condition.into();
            let parsed = NativeLayout::try_from(&wire).unwrap();
            let tile = get(&parsed, "tile");
            assert_inert(tile);
            assert_eq!((tile.viewport_min_width, tile.viewport_max_width), (0, 0));
        }
        wire.version = 1;
        wire.nodes[3].screen_visibility = wire::NativeScreenVisibility {
            max_width: 767,
            ..Default::default()
        }
        .into();
        assert_inert(get(&NativeLayout::try_from(&wire).unwrap(), "tile"));
    }

    #[test]
    fn extended_forest_preserves_text_recipes_and_explicit_empty_confirmation() {
        let wire = extended_forest();
        let parsed = NativeLayout::try_from(&wire).unwrap();
        assert_eq!(parsed.version, 2);
        let node = get(&parsed, "recipe");
        assert_eq!(node.title, "Plain text\n<script>not executed</script>");
        assert_eq!(node.secondary, "Server-projected text");
        let interaction = &node.recipes.as_ref().unwrap().interactions[&Gesture::Tap];
        assert_eq!(interaction.confirmation.as_deref(), Some(""));
        assert!(matches!(interaction.intention, Intention::MoreInfo(_)));
        assert_eq!(
            get(&parsed, "chip").recipe_kind,
            Some(RecipeKind::TemplateChip)
        );
    }
    #[test]
    fn popup_body_is_only_traversed_from_its_explicit_root() {
        let parsed = NativeLayout::try_from(&extended_forest()).unwrap();
        let home = parsed.ordered_nodes(&identity("view").unwrap());
        assert!(
            !home
                .iter()
                .any(|node| matches!(node.id.as_str(), "popup" | "popup-tile"))
        );
        let popup = parsed.ordered_nodes(&identity("popup").unwrap());
        assert_eq!(
            popup
                .iter()
                .map(|node| node.id.as_str())
                .collect::<Vec<_>>(),
            ["popup", "popup-tile"]
        );
    }
    #[test]
    fn invalid_recipe_roles_missing_bindings_and_unknown_features_are_sourced_inert_nodes() {
        let base = extended_forest();
        let recipe_index = base
            .nodes
            .iter()
            .position(|node| node.id == "recipe")
            .unwrap();
        let mut cases = vec![base; 9];
        cases[0].nodes[recipe_index].recipe_kind = wire::NativeRecipeKind::BubblePopup.into();
        cases[1].nodes[recipe_index].recipe_kind = wire::NativeRecipeKind::Select.into();
        cases[2].nodes[recipe_index].recipe_kind = wire::NativeRecipeKind::Alarm.into();
        cases[3].nodes[recipe_index].recipe_kind = wire::NativeRecipeKind::Entity.into();
        cases[4].nodes[recipe_index].recipe_kind = EnumValue::Unknown(99);
        cases[5].nodes[recipe_index].features = vec![wire::NativeFeatureKind::Toggle.into()];
        cases[6].nodes[recipe_index].features = vec![EnumValue::Unknown(99)];
        cases[7].nodes[recipe_index]
            .recipes
            .as_option_mut()
            .unwrap()
            .version = 99;
        cases[8].nodes[recipe_index].control_presentation = EnumValue::Unknown(99);
        for wire in cases {
            let parsed = NativeLayout::try_from(&wire).unwrap();
            assert_inert(get(&parsed, "recipe"));
            assert_eq!(get(&parsed, "tile").kind, Kind::Tile);
        }
        for kind in [
            wire::NativeRecipeKind::Select,
            wire::NativeRecipeKind::Alarm,
            wire::NativeRecipeKind::Entity,
        ] {
            let mut wire = extended_forest();
            wire.nodes[recipe_index].recipe_kind = kind.into();
            wire.nodes[recipe_index].entity_id = "synthetic-entity".into();
            assert_eq!(
                get(&NativeLayout::try_from(&wire).unwrap(), "recipe").kind,
                Kind::Recipe
            );
        }
    }
    #[test]
    fn extended_parent_and_nested_unknowns_clear_all_descendant_intentions() {
        use buffa::Message;
        let mut wire = extended_forest();
        wire.nodes[2].kind = EnumValue::Unknown(99);
        let parsed = NativeLayout::try_from(&wire).unwrap();
        for id in [
            "grid",
            "recipe",
            "tile",
            "popup",
            "popup-tile",
            "chips",
            "chip",
            "vertical",
            "text",
        ] {
            assert_inert(get(&parsed, id));
        }
        let mut wire = extended_forest();
        let recipe = wire
            .nodes
            .iter_mut()
            .find(|node| node.id == "recipe")
            .unwrap();
        let mut bytes = recipe.source.as_option().unwrap().encode_to_vec();
        bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
        recipe.source = wire::LayoutSource::decode_from_slice(&bytes)
            .unwrap()
            .into();
        assert_inert(get(&NativeLayout::try_from(&wire).unwrap(), "recipe"));
    }
    #[test]
    fn features_require_a_tile_unique_known_values_and_a_bounded_list() {
        let mut wire = extended_forest();
        wire.nodes[3].features = vec![
            wire::NativeFeatureKind::Toggle.into(),
            wire::NativeFeatureKind::Brightness.into(),
        ];
        let parsed = NativeLayout::try_from(&wire).unwrap();
        assert_eq!(
            get(&parsed, "tile").features,
            [FeatureKind::Toggle, FeatureKind::Brightness]
        );
        wire.nodes[3]
            .features
            .push(wire::NativeFeatureKind::Toggle.into());
        assert_inert(get(&NativeLayout::try_from(&wire).unwrap(), "tile"));
        wire.nodes[3].features = vec![wire::NativeFeatureKind::Toggle.into(); 17];
        assert_eq!(NativeLayout::try_from(&wire), Err(LayoutFailure::Budget));
    }
    #[test]
    fn entity_bindings_are_bounded_and_cannot_contain_control_characters() {
        for invalid in ["entity\nwith-control".to_owned(), "e".repeat(4097)] {
            let mut wire = extended_forest();
            wire.nodes[3].entity_id = invalid.clone();
            assert_eq!(NativeLayout::try_from(&wire), Err(LayoutFailure::Invalid));
            let mut wire = extended_forest();
            wire.nodes
                .iter_mut()
                .find(|node| node.id == "recipe")
                .unwrap()
                .entity_id = invalid;
            assert_eq!(NativeLayout::try_from(&wire), Err(LayoutFailure::Invalid));
        }
    }
    #[test]
    fn new_containers_cannot_smuggle_structural_nodes_into_cards_or_chips() {
        let mut wire = extended_forest();
        let chip = wire
            .nodes
            .iter_mut()
            .find(|node| node.id == "chip")
            .unwrap();
        chip.kind = wire::LayoutKind::Tile.into();
        chip.recipe_kind = wire::NativeRecipeKind::Unspecified.into();
        chip.entity_id = "synthetic-entity".into();
        assert_eq!(NativeLayout::try_from(&wire), Err(LayoutFailure::Invalid));
        let mut wire = extended_forest();
        let text = wire
            .nodes
            .iter_mut()
            .find(|node| node.id == "text")
            .unwrap();
        text.kind = wire::LayoutKind::Section.into();
        text.recipe_kind = wire::NativeRecipeKind::Unspecified.into();
        assert_eq!(NativeLayout::try_from(&wire), Err(LayoutFailure::Invalid));
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
    fn unnegotiated_recipes_cannot_be_ignored_in_favor_of_a_default_tile_action() {
        let mut wire = forest();
        wire.nodes[3].recipes = wire::NativeRecipes {
            version: 1,
            ..Default::default()
        }
        .into();
        let parsed = NativeLayout::try_from(&wire).unwrap();
        let node = &parsed.nodes[&identity("tile").unwrap()];
        assert_eq!(node.kind, Kind::Unsupported);
        assert!(node.entity.is_none() && !node.diagnostic.is_empty());
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
