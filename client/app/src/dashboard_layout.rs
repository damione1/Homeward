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
pub(crate) struct CoverLock {
    pub capability: CapabilityName,
    pub label: String,
}
// Only strictly admitted finite definitions enter this wrapper, so equality
// is reflexive even though the underlying transport uses f64.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct Modules(pub Vec<home_client_model::control_modules::ControlModule>);
impl Eq for Modules {}

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
    pub control_family: Option<home_client_model::control_modules::VisualFamily>,
    pub control_modules: Modules,
    pub summary_only: bool,
    pub entity: Option<EntityName>,
    pub capability: Option<CapabilityName>,
    pub cover_lock: Option<CoverLock>,
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
        EnumValue::Unknown(_) => return Err(LayoutFailure::Unsupported),
    };
    let mut node = Node {
        id: identity(&raw.id)?,
        route: None,
        kind: Kind::Unsupported,
        icon,
        source: Source {
            file: source.file.clone(),
            line: u32::try_from(source.line).map_err(|_| LayoutFailure::Invalid)?,
            column: u32::try_from(source.column).map_err(|_| LayoutFailure::Invalid)?,
            path: source.path.clone(),
            variant: source.variant.clone(),
        },
        title: raw.display_name.clone(),
        secondary: String::new(),
        recipe_kind: None,
        recipes: None,
        features: Vec::new(),
        control_family: None,
        control_modules: Modules::default(),
        summary_only: false,
        entity: None,
        capability: None,
        cover_lock: None,
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
                EnumValue::Known(wire::DashboardHeadingStyle::Unspecified)
                | EnumValue::Unknown(_) => return Err(LayoutFailure::Unsupported),
            };
            interactions = v.interactions.as_option()
        }
        C::Tile(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            };
            node.kind = Kind::Tile;
            modular_binding(&mut node, v.visual_family, &v.modules)?;
            bind_node(&mut node, &v.capability)?;
            node.cover_lock = v
                .cover_lock
                .as_option()
                .map(|lock| {
                    let capability = CapabilityName::try_from(lock.capability.as_str())
                        .map_err(|_| LayoutFailure::Invalid)?;
                    if !lock.__buffa_unknown_fields.is_empty()
                        || lock.display_name.len() > 256
                        || lock.display_name.contains('\0')
                        || node.capability.as_ref().is_none_or(|source| {
                            source.home() != capability.home() || source == &capability
                        })
                    {
                        return Err(LayoutFailure::Invalid);
                    }
                    Ok(CoverLock {
                        capability,
                        label: lock.display_name.clone(),
                    })
                })
                .transpose()?;
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
            modular_binding(&mut node, v.visual_family, &v.modules)?;
            node.recipe_kind = recipe_kind(v.kind).map_err(|_| LayoutFailure::Unsupported)?;
            if node.recipe_kind.is_none() {
                return Err(LayoutFailure::Unsupported);
            };
            if !v.capability.is_empty() {
                bind_node(&mut node, &v.capability)?
            };
            node.cover_lock = v
                .cover_lock
                .as_option()
                .map(|lock| {
                    let capability = CapabilityName::try_from(lock.capability.as_str())
                        .map_err(|_| LayoutFailure::Invalid)?;
                    if !lock.__buffa_unknown_fields.is_empty()
                        || lock.display_name.len() > 256
                        || lock.display_name.contains('\0')
                        || node.capability.as_ref().is_none_or(|source| {
                            source.home() != capability.home() || source == &capability
                        })
                    {
                        return Err(LayoutFailure::Invalid);
                    }
                    Ok(CoverLock {
                        capability,
                        label: lock.display_name.clone(),
                    })
                })
                .transpose()?;
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
        EnumValue::Known(wire::DashboardControlPresentation::Unspecified)
        | EnumValue::Unknown(_) => Err(LayoutFailure::Unsupported),
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

fn modular_binding(
    node: &mut Node,
    family: buffa::EnumValue<wire::ControlVisualFamily>,
    modules: &[wire::ControlModule],
) -> Result<(), LayoutFailure> {
    use home_wire_contract::control_modules_contract as codec;
    if modules.is_empty() && family.as_known() == Some(wire::ControlVisualFamily::Unspecified) {
        return Ok(());
    }
    if modules.len() > 16 {
        return Err(LayoutFailure::Invalid);
    }
    let family = codec::visual_family(family).map_err(|_| LayoutFailure::Unsupported)?;
    let parsed = modules
        .iter()
        .map(codec::module)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| LayoutFailure::Invalid)?;
    let mut seen = std::collections::HashSet::new();
    for module in modules {
        if !seen.insert((module.capability.clone(), module.role.to_i32())) {
            return Err(LayoutFailure::Invalid);
        }
    }
    node.control_family = Some(family);
    node.control_modules = Modules(parsed);
    Ok(())
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use buffa::Message;
    use home_client_model::dashboard::{Gesture, Intention};
    use wire::dashboard_layout_node::Content as C;
    pub(crate) const ENTITY: &str = "homes/synthetic/entities/synthetic-entity";
    pub(crate) const CAPABILITY: &str =
        "homes/synthetic/entities/synthetic-entity/capabilities/power";
    pub(crate) fn node(id: &str, content: C) -> wire::DashboardLayoutNode {
        wire::DashboardLayoutNode {
            id: id.into(),
            source: wire::DashboardLayoutSource {
                file: "synthetic.yaml".into(),
                path: id.into(),
                variant: "synthetic".into(),
                line: 1,
                ..Default::default()
            }
            .into(),
            content: Some(content),
            ..Default::default()
        }
    }
    pub(crate) fn children(ids: &[&str]) -> wire::DashboardChildren {
        wire::DashboardChildren {
            children: ids.iter().map(|id| (*id).into()).collect(),
            ..Default::default()
        }
    }
    pub(crate) fn tile(id: &str) -> wire::DashboardLayoutNode {
        node(
            id,
            C::Tile(
                wire::DashboardTileNode {
                    capability: CAPABILITY.into(),
                    presentation: wire::DashboardControlPresentation::Controls.into(),
                    ..Default::default()
                }
                .into(),
            ),
        )
    }
    fn grid(id: &str, ids: &[&str]) -> wire::DashboardLayoutNode {
        node(
            id,
            C::Grid(
                wire::DashboardGridNode {
                    children: children(ids).into(),
                    ..Default::default()
                }
                .into(),
            ),
        )
    }
    fn recipe(id: &str, kind: wire::DashboardRecipeKind) -> wire::DashboardLayoutNode {
        node(
            id,
            C::Recipe(
                wire::DashboardRecipeNode {
                    kind: kind.into(),
                    presentation: wire::DashboardControlPresentation::Controls.into(),
                    ..Default::default()
                }
                .into(),
            ),
        )
    }
    pub(crate) fn forest() -> wire::DashboardLayout {
        wire::DashboardLayout {
            views: vec!["view".into()],
            nodes: vec![
                node(
                    "view",
                    C::View(
                        wire::DashboardViewNode {
                            children: children(&["section"]).into(),
                            ..Default::default()
                        }
                        .into(),
                    ),
                ),
                node(
                    "section",
                    C::Section(
                        wire::DashboardSectionNode {
                            children: children(&["grid"]).into(),
                            ..Default::default()
                        }
                        .into(),
                    ),
                ),
                grid("grid", &["tile", "unknown"]),
                tile("tile"),
                node(
                    "unknown",
                    C::Unsupported(
                        wire::DashboardUnsupportedNode {
                            diagnostic: "Unsupported source item".into(),
                            ..Default::default()
                        }
                        .into(),
                    ),
                ),
            ],
            ..Default::default()
        }
    }
    fn grid_children(value: &mut wire::DashboardLayoutNode) -> &mut Vec<String> {
        let Some(C::Grid(v)) = &mut value.content else {
            panic!("grid")
        };
        &mut v.children.get_or_insert_default().children
    }
    fn recipe_mut(value: &mut wire::DashboardLayoutNode) -> &mut wire::DashboardRecipeNode {
        let Some(C::Recipe(v)) = &mut value.content else {
            panic!("recipe")
        };
        v
    }
    fn tile_mut(value: &mut wire::DashboardLayoutNode) -> &mut wire::DashboardTileNode {
        let Some(C::Tile(v)) = &mut value.content else {
            panic!("tile")
        };
        v
    }
    pub(crate) fn extended_forest() -> wire::DashboardLayout {
        let mut value = forest();
        grid_children(&mut value.nodes[2])
            .extend(["recipe", "popup", "chips", "vertical"].map(String::from));
        let mut r = recipe("recipe", wire::DashboardRecipeKind::Template);
        r.display_name = "Plain text\n<script>not executed</script>".into();
        let source = r.source.clone();
        let body = recipe_mut(&mut r);
        body.secondary = "Server-projected text".into();
        body.interactions = wire::DashboardInteractions {
            interactions: vec![wire::DashboardInteraction {
                gesture: wire::DashboardGesture::Tap.into(),
                intention: Some(wire::dashboard_interaction::Intention::MoreInfo(
                    wire::DashboardEntityTarget {
                        entity: ENTITY.into(),
                        ..Default::default()
                    }
                    .into(),
                )),
                confirmation: wire::DashboardConfirmation::default().into(),
                source,
                ..Default::default()
            }],
            ..Default::default()
        }
        .into();
        let mut text = recipe("text", wire::DashboardRecipeKind::Text);
        text.display_name = "Bounded plain text".into();
        value.nodes.extend([
            r,
            node(
                "popup",
                C::Popup(
                    wire::DashboardPopupNode {
                        children: children(&["popup-tile"]).into(),
                        ..Default::default()
                    }
                    .into(),
                ),
            ),
            node(
                "chips",
                C::ChipGroup(
                    wire::DashboardChipGroupNode {
                        children: children(&["chip"]).into(),
                        ..Default::default()
                    }
                    .into(),
                ),
            ),
            recipe("chip", wire::DashboardRecipeKind::TemplateChip),
            tile("popup-tile"),
            node(
                "vertical",
                C::VerticalStack(
                    wire::DashboardStackNode {
                        children: children(&["text"]).into(),
                        ..Default::default()
                    }
                    .into(),
                ),
            ),
            text,
        ]);
        value
    }
    fn get<'a>(layout: &'a NativeLayout, id: &str) -> &'a Node {
        &layout.nodes[&identity(id).unwrap()]
    }
    fn assert_inert(node: &Node) {
        assert_eq!(node.kind, Kind::Unsupported);
        assert!(node.entity.is_none() && node.capability.is_none() && node.recipes.is_none());
        assert!(node.features.is_empty() && node.recipe_kind.is_none());
        assert!(!node.diagnostic.is_empty());
        assert_eq!(node.source.file, "synthetic.yaml");
    }
    #[test]
    fn screen_bounds_are_presentation_data_and_unknown_or_invalid_bounds_refuse_layout() {
        let mut value = forest();
        value.nodes[3].screen_visibility = wire::DashboardScreenVisibility {
            min_width: 768,
            max_width: 1199,
            ..Default::default()
        }
        .into();
        let parsed = NativeLayout::try_from(&value).unwrap();
        let tile = get(&parsed, "tile");
        assert_eq!(
            (tile.viewport_min_width, tile.viewport_max_width),
            (768, 1199)
        );
        assert_eq!(tile.kind, Kind::Tile);
        assert!(tile.entity.is_some());
        for condition in [
            wire::DashboardScreenVisibility {
                min_width: 1200,
                max_width: 767,
                ..Default::default()
            },
            wire::DashboardScreenVisibility {
                min_width: 65536,
                ..Default::default()
            },
            wire::DashboardScreenVisibility::decode_from_slice(&[24, 1]).unwrap(),
        ] {
            value.nodes[3].screen_visibility = condition.into();
            assert!(NativeLayout::try_from(&value).is_err());
        }
    }
    #[test]
    fn extended_forest_preserves_text_recipes_and_explicit_empty_confirmation() {
        let value = extended_forest();
        let parsed = NativeLayout::try_from(&value).unwrap();
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
        assert_eq!(
            parsed
                .ordered_nodes(&identity("popup").unwrap())
                .iter()
                .map(|node| node.id.as_str())
                .collect::<Vec<_>>(),
            ["popup", "popup-tile"]
        );
    }
    #[test]
    fn unknown_recipes_features_and_interactions_never_admit_an_action() {
        let base = extended_forest();
        let index = base.nodes.iter().position(|n| n.id == "recipe").unwrap();
        let mut cases = vec![base; 4];
        recipe_mut(&mut cases[0].nodes[index]).kind = EnumValue::Unknown(99);
        recipe_mut(&mut cases[1].nodes[index]).features = vec![EnumValue::Unknown(99)];
        recipe_mut(&mut cases[2].nodes[index]).presentation = EnumValue::Unknown(99);
        let interactions = &mut recipe_mut(&mut cases[3].nodes[index]).interactions;
        let mut bytes = interactions.encode_to_vec();
        bytes.extend_from_slice(&[0x98, 0x06, 0x01]);
        *interactions = wire::DashboardInteractions::decode_from_slice(&bytes)
            .unwrap()
            .into();
        for value in cases {
            assert!(NativeLayout::try_from(&value).is_err());
        }
        // Missing binding never manufactures a target for display-only recipes.
        let parsed = NativeLayout::try_from(&extended_forest()).unwrap();
        assert!(
            get(&parsed, "recipe").entity.is_none() && get(&parsed, "recipe").capability.is_none()
        );
        for kind in [
            wire::DashboardRecipeKind::Select,
            wire::DashboardRecipeKind::Alarm,
            wire::DashboardRecipeKind::Entity,
        ] {
            let mut value = extended_forest();
            let r = recipe_mut(&mut value.nodes[index]);
            r.kind = kind.into();
            r.capability = CAPABILITY.into();
            assert_eq!(
                get(&NativeLayout::try_from(&value).unwrap(), "recipe").kind,
                Kind::Recipe
            );
        }
    }
    #[test]
    fn unsupported_parent_clears_descendant_intentions_and_unknown_nested_fields_refuse_layout() {
        let mut value = extended_forest();
        let descendants = grid_children(&mut value.nodes[2]).clone();
        value.nodes[2].content = Some(C::Unsupported(
            wire::DashboardUnsupportedNode {
                children: descendants,
                diagnostic: "Unsupported source parent".into(),
                ..Default::default()
            }
            .into(),
        ));
        let parsed = NativeLayout::try_from(&value).unwrap();
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
        let mut value = extended_forest();
        let r = value.nodes.iter_mut().find(|n| n.id == "recipe").unwrap();
        let mut bytes = r.source.encode_to_vec();
        bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
        r.source = wire::DashboardLayoutSource::decode_from_slice(&bytes)
            .unwrap()
            .into();
        assert!(NativeLayout::try_from(&value).is_err());
    }
    #[test]
    fn features_are_unique_known_values_and_have_a_bounded_list() {
        let mut value = extended_forest();
        tile_mut(&mut value.nodes[3]).features = vec![
            wire::DashboardFeatureKind::Toggle.into(),
            wire::DashboardFeatureKind::Brightness.into(),
        ];
        assert_eq!(
            get(&NativeLayout::try_from(&value).unwrap(), "tile").features,
            [FeatureKind::Toggle, FeatureKind::Brightness]
        );
        tile_mut(&mut value.nodes[3])
            .features
            .push(wire::DashboardFeatureKind::Toggle.into());
        assert_eq!(NativeLayout::try_from(&value), Err(LayoutFailure::Invalid));
        tile_mut(&mut value.nodes[3]).features =
            vec![wire::DashboardFeatureKind::Toggle.into(); 17];
        assert_eq!(NativeLayout::try_from(&value), Err(LayoutFailure::Budget));
    }
    #[test]
    fn capability_bindings_are_bounded_strict_and_cannot_contain_control_characters() {
        for invalid in [
            "entity\nwith-control".to_owned(),
            "e".repeat(4097),
            ENTITY.into(),
        ] {
            let mut value = extended_forest();
            tile_mut(&mut value.nodes[3]).capability = invalid.clone();
            assert_eq!(NativeLayout::try_from(&value), Err(LayoutFailure::Invalid));
            let mut value = extended_forest();
            let r = value.nodes.iter_mut().find(|n| n.id == "recipe").unwrap();
            recipe_mut(r).capability = invalid;
            assert_eq!(NativeLayout::try_from(&value), Err(LayoutFailure::Invalid));
        }
    }
    #[test]
    fn containers_cannot_smuggle_structural_nodes_into_cards_or_chips() {
        let mut value = extended_forest();
        let chip = value.nodes.iter_mut().find(|n| n.id == "chip").unwrap();
        chip.content = tile("chip").content;
        assert_eq!(NativeLayout::try_from(&value), Err(LayoutFailure::Invalid));
        let mut value = extended_forest();
        let text = value.nodes.iter_mut().find(|n| n.id == "text").unwrap();
        text.content = Some(C::Section(Box::default()));
        assert_eq!(NativeLayout::try_from(&value), Err(LayoutFailure::Invalid));
    }
    #[test]
    fn malformed_forests_never_turn_cycles_shared_or_orphaned_data_into_widgets() {
        let mut cases = vec![forest(); 5];
        grid_children(&mut cases[0].nodes[2]).push("view".into());
        let Some(C::Section(section)) = &mut cases[1].nodes[1].content else {
            panic!()
        };
        section
            .children
            .get_or_insert_default()
            .children
            .push("tile".into());
        cases[2].nodes.push(tile("orphan"));
        grid_children(&mut cases[3].nodes[2]).push("missing".into());
        cases[4].views.push("view".into());
        for value in cases {
            assert_eq!(NativeLayout::try_from(&value), Err(LayoutFailure::Invalid));
        }
        assert!(NativeLayout::try_from(&forest()).is_ok());
    }
    #[test]
    fn unknown_content_never_falls_back_to_a_default_tile_action() {
        let mut value = forest();
        value.nodes[3].content = None;
        assert_eq!(
            NativeLayout::try_from(&value),
            Err(LayoutFailure::Unsupported)
        );
        let mut value = forest();
        let mut bytes = value.nodes[3].encode_to_vec();
        bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
        value.nodes[3] = wire::DashboardLayoutNode::decode_from_slice(&bytes).unwrap();
        assert_eq!(
            NativeLayout::try_from(&value),
            Err(LayoutFailure::Unsupported)
        );
    }
    #[test]
    fn parser_accepts_the_full_importer_card_depth_and_rejects_more() {
        let mut value = forest();
        value.nodes.truncate(2);
        let Some(C::Section(section)) = &mut value.nodes[1].content else {
            panic!()
        };
        section.children = children(&["card0"]).into();
        for level in 0..=16 {
            let id = format!("card{level}");
            let next = format!("card{}", level + 1);
            value.nodes.push(if level == 16 {
                tile(&id)
            } else {
                grid(&id, &[&next])
            });
        }
        assert!(NativeLayout::try_from(&value).is_ok());
        *value.nodes.last_mut().unwrap() = grid("card16", &["overflow"]);
        value.nodes.push(tile("overflow"));
        assert_eq!(NativeLayout::try_from(&value), Err(LayoutFailure::Budget));
    }
}
