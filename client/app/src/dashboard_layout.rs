//! Validated layout forest. Server source identities are never row indices.
use crate::dashboard::LayoutFailure;
use buffa::EnumValue;
use home_client_model::EntityId;
pub(crate) use home_client_model::dashboard::{NodeId, Source};
use home_client_model::dashboard::{Profile, Recipes};
use home_wire_contract::dashboard_contract::{RecipeFailure, decode_recipes};
use home_wire_contract::home::dashboard::v1 as wire;
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
    pub source: Source,
    pub title: String,
    pub secondary: String,
    pub recipe_kind: Option<RecipeKind>,
    pub recipes: Option<Recipes>,
    pub features: Vec<FeatureKind>,
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
impl Node {
    pub fn device_card(&self) -> bool {
        match self.kind {
            Kind::Tile => true,
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
    pub version: u32,
    pub views: Vec<NodeId>,
    pub nodes: BTreeMap<NodeId, Node>,
}
fn bounded(value: &str, empty: bool, maximum: usize) -> bool {
    (empty || !value.is_empty()) && value.len() <= maximum && !value.chars().any(char::is_control)
}
fn identity(value: &str) -> Result<NodeId, LayoutFailure> {
    NodeId::parse(value).map_err(|_| LayoutFailure::Invalid)
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
        EnumValue::Known(wire::LayoutKind::Recipe) => Kind::Recipe,
        EnumValue::Known(wire::LayoutKind::VerticalStack) => Kind::VerticalStack,
        EnumValue::Known(wire::LayoutKind::Popup) => Kind::Popup,
        EnumValue::Known(wire::LayoutKind::ChipGroup) => Kind::ChipGroup,
        EnumValue::Known(wire::LayoutKind::Unsupported | wire::LayoutKind::Unspecified)
        | EnumValue::Unknown(_) => Kind::Unsupported,
    }
}
fn recipe_kind(value: EnumValue<wire::NativeRecipeKind>) -> Result<Option<RecipeKind>, ()> {
    Ok(match value {
        EnumValue::Known(wire::NativeRecipeKind::Unspecified) => None,
        EnumValue::Known(wire::NativeRecipeKind::Template) => Some(RecipeKind::Template),
        EnumValue::Known(wire::NativeRecipeKind::Select) => Some(RecipeKind::Select),
        EnumValue::Known(wire::NativeRecipeKind::Alarm) => Some(RecipeKind::Alarm),
        EnumValue::Known(wire::NativeRecipeKind::BubbleButton) => Some(RecipeKind::BubbleButton),
        EnumValue::Known(wire::NativeRecipeKind::BubblePopup) => Some(RecipeKind::BubblePopup),
        EnumValue::Known(wire::NativeRecipeKind::Chips) => Some(RecipeKind::Chips),
        EnumValue::Known(wire::NativeRecipeKind::TemplateChip) => Some(RecipeKind::TemplateChip),
        EnumValue::Known(wire::NativeRecipeKind::Button) => Some(RecipeKind::Button),
        EnumValue::Known(wire::NativeRecipeKind::Entity) => Some(RecipeKind::Entity),
        EnumValue::Known(wire::NativeRecipeKind::Text) => Some(RecipeKind::Text),
        EnumValue::Unknown(_) => return Err(()),
    })
}
fn feature_kind(value: EnumValue<wire::NativeFeatureKind>) -> Option<FeatureKind> {
    match value {
        EnumValue::Known(wire::NativeFeatureKind::Toggle) => Some(FeatureKind::Toggle),
        EnumValue::Known(wire::NativeFeatureKind::Brightness) => Some(FeatureKind::Brightness),
        EnumValue::Known(wire::NativeFeatureKind::ColorTemperature) => {
            Some(FeatureKind::ColorTemperature)
        }
        EnumValue::Known(wire::NativeFeatureKind::TargetTemperature) => {
            Some(FeatureKind::TargetTemperature)
        }
        EnumValue::Known(wire::NativeFeatureKind::ClimateModes) => Some(FeatureKind::ClimateModes),
        EnumValue::Known(wire::NativeFeatureKind::FanSpeed) => Some(FeatureKind::FanSpeed),
        EnumValue::Known(wire::NativeFeatureKind::NumericInput) => Some(FeatureKind::NumericInput),
        EnumValue::Known(wire::NativeFeatureKind::CoverOpenClose) => {
            Some(FeatureKind::CoverOpenClose)
        }
        EnumValue::Known(wire::NativeFeatureKind::CoverPosition) => {
            Some(FeatureKind::CoverPosition)
        }
        EnumValue::Known(wire::NativeFeatureKind::AlarmModes) => Some(FeatureKind::AlarmModes),
        EnumValue::Known(wire::NativeFeatureKind::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
fn recipe_shape(node: &Node) -> bool {
    if !node.features.is_empty() && node.kind != Kind::Tile {
        return false;
    }
    match node.kind {
        Kind::Recipe => match node.recipe_kind {
            Some(RecipeKind::Select | RecipeKind::Alarm | RecipeKind::Entity) => {
                node.entity.is_some()
            }
            Some(
                RecipeKind::Template
                | RecipeKind::BubbleButton
                | RecipeKind::TemplateChip
                | RecipeKind::Button
                | RecipeKind::Text,
            ) => true,
            Some(RecipeKind::BubblePopup | RecipeKind::Chips) | None => false,
        },
        Kind::Popup => node.recipe_kind == Some(RecipeKind::BubblePopup),
        Kind::ChipGroup => node.recipe_kind == Some(RecipeKind::Chips),
        Kind::View
        | Kind::Section
        | Kind::Grid
        | Kind::HorizontalStack
        | Kind::VerticalStack
        | Kind::Heading
        | Kind::Tile => node.recipe_kind.is_none(),
        Kind::Unsupported => true,
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
            Self::Heading | Self::Tile | Self::Recipe => false,
        }
    }
}
impl Node {
    pub(crate) fn inert(&mut self, diagnostic: &str) {
        self.kind = Kind::Unsupported;
        self.entity = None;
        self.recipes = None;
        self.features.clear();
        self.recipe_kind = None;
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
impl TryFrom<&wire::NativeLayout> for NativeLayout {
    type Error = LayoutFailure;
    fn try_from(value: &wire::NativeLayout) -> Result<Self, Self::Error> {
        if value.nodes.len() > MAX_NODES || value.views.len() > 128 {
            return Err(LayoutFailure::Budget);
        }
        if value.views.is_empty() || value.nodes.is_empty() {
            return Err(LayoutFailure::Invalid);
        }
        let profile = match value.version {
            1 => Some(Profile::Basic),
            2 => Some(Profile::Recipes),
            _ => None,
        };
        let future = profile.is_none() || !value.__buffa_unknown_fields.is_empty();
        let mut nodes = BTreeMap::new();
        let mut child_count = 0usize;
        for node in &value.nodes {
            let id = identity(&node.id)?;
            let source = node.source.as_option().ok_or(LayoutFailure::Invalid)?;
            if !bounded(&source.file, false, 4096)
                || !bounded(&source.path, false, 4096)
                || !bounded(&source.variant, false, 4096)
                || node.title.len() > 4096
                || node.title.contains('\0')
                || node.secondary.len() > 512
                || node.secondary.contains('\0')
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
                secondary: node.secondary.clone(),
                recipe_kind: None,
                recipes: None,
                features: Vec::new(),
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
                || (profile == Some(Profile::Basic)
                    && (node.recipes.as_option().is_some()
                        || node.recipe_kind
                            != EnumValue::Known(wire::NativeRecipeKind::Unspecified)
                        || !node.secondary.is_empty()
                        || !node.features.is_empty()
                        || matches!(
                            parsed.kind,
                            Kind::Recipe | Kind::VerticalStack | Kind::Popup | Kind::ChipGroup
                        )))
            {
                parsed.kind = Kind::Unsupported;
            }
            if profile == Some(Profile::Recipes) && parsed.kind != Kind::Unsupported {
                match recipe_kind(node.recipe_kind) {
                    Ok(kind) => parsed.recipe_kind = kind,
                    Err(()) => parsed.kind = Kind::Unsupported,
                }
                if node.features.len() > 16 {
                    return Err(LayoutFailure::Budget);
                }
                let mut features = BTreeSet::new();
                for value in &node.features {
                    match feature_kind(*value) {
                        Some(feature) if features.insert(feature) => parsed.features.push(feature),
                        Some(_) | None => {
                            parsed.kind = Kind::Unsupported;
                        }
                    }
                }
                if let Some(recipes) = node.recipes.as_option() {
                    match decode_recipes(recipes) {
                        Ok(recipes) => parsed.recipes = Some(recipes),
                        Err(RecipeFailure::Budget) => return Err(LayoutFailure::Budget),
                        Err(RecipeFailure::Invalid | RecipeFailure::Unsupported) => {
                            parsed.kind = Kind::Unsupported
                        }
                    }
                }
            }
            if parsed.kind == Kind::Tile
                || (parsed.kind == Kind::Recipe && !node.entity_id.is_empty())
            {
                if !bounded(&node.entity_id, false, 4096) {
                    return Err(LayoutFailure::Invalid);
                }
                parsed.entity = Some(node.entity_id.parse().map_err(|_| LayoutFailure::Invalid)?);
                if !parsed.children.is_empty() {
                    return Err(LayoutFailure::Invalid);
                }
            } else if !node.entity_id.is_empty() && parsed.kind != Kind::Unsupported {
                return Err(LayoutFailure::Invalid);
            }
            if matches!(parsed.kind, Kind::Heading | Kind::Recipe) && !parsed.children.is_empty() {
                return Err(LayoutFailure::Invalid);
            }
            if !recipe_shape(&parsed) {
                parsed.kind = Kind::Unsupported;
            }
            if parsed.kind == Kind::Unsupported {
                parsed.inert(if future {
                    "Version du layout non prise en charge."
                } else {
                    "Élément ou propriété non pris en charge."
                });
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
                node.inert("Élément inactif : parent non pris en charge.");
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
        Ok(Self {
            version: value.version,
            views,
            nodes,
        })
    }
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
        let mut cases = vec![base; 8];
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
