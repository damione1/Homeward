use crate::{
    ContentItem, HomewardWindow, OptionRow, ProGrocerySuggestion, ProTaskAccess, ProTaskPhase,
    ProTaskRow,
    list_host::item_text,
    list_state::{Draft, DraftPhase, Freshness, State},
};
use home_client_cache::lists::{CapturedRows, ListReadMetadata, ListReadSnapshot};
use home_client_model::lists::ItemKey;
use slint::{Model, ModelNotify, ModelRc, ModelTracker};
use std::{collections::BTreeMap, sync::Arc};

#[derive(Clone)]
pub(super) struct Projection {
    generation: i32,
    privacy_generation: i32,
    list_token: i32,
    active: bool,
    catalog: Option<Arc<ListReadSnapshot>>,
    items: Option<Arc<ListReadSnapshot>>,
    catalog_tokens: Arc<Vec<i32>>,
    item_tokens: Arc<Vec<i32>>,
    catalog_phase: Freshness,
    item_phase: Freshness,
    selected: Option<ItemKey>,
    drafts: BTreeMap<ItemKey, Draft>,
    busy: bool,
    notice: String,
    confirmed_add: Option<(u64, String)>,
}
impl Projection {
    pub fn new(state: &State) -> Self {
        Self {
            generation: state.generation,
            privacy_generation: state.privacy_generation,
            list_token: state.selected_list_token(),
            active: state.active && state.foreground && state.interactive,
            catalog: state.catalog.clone(),
            items: state.items.clone(),
            catalog_tokens: state.catalog_tokens.clone(),
            item_tokens: state.item_tokens.clone(),
            catalog_phase: state.catalog_phase,
            item_phase: state.item_phase,
            selected: state.selected_item.clone(),
            drafts: state.drafts.clone(),
            busy: state.busy(),
            confirmed_add: state.confirmed_add.clone(),
            notice: [state.read_notice, state.notice]
                .into_iter()
                .filter(|text| !text.is_empty())
                .collect::<Vec<_>>()
                .join(" "),
        }
    }
    pub(super) fn same(&self, other: &Self) -> bool {
        self.generation == other.generation
            && self.privacy_generation == other.privacy_generation
            && self.list_token == other.list_token
            && self.active == other.active
            && same_snapshot(&self.catalog, &other.catalog)
            && same_snapshot(&self.items, &other.items)
            && self.catalog_tokens == other.catalog_tokens
            && self.item_tokens == other.item_tokens
            && self.catalog_phase == other.catalog_phase
            && self.item_phase == other.item_phase
            && self.selected == other.selected
            && self.drafts == other.drafts
            && self.busy == other.busy
            && self.notice == other.notice
            && self.confirmed_add == other.confirmed_add
    }
    fn same_tasks(&self, other: &Self) -> bool {
        self.generation == other.generation
            && same_snapshot(&self.items, &other.items)
            && self.item_tokens == other.item_tokens
            && self.busy == other.busy
            && (!self.busy || self.selected == other.selected)
            && self.drafts.len() == other.drafts.len()
            && self.drafts.iter().all(|(key, draft)| {
                other
                    .drafts
                    .get(key)
                    .is_some_and(|value| value.phase == draft.phase)
            })
    }
    fn same_catalog(&self, other: &Self) -> bool {
        self.generation == other.generation
            && self.active == other.active
            && self.busy == other.busy
            && self.catalog_phase == other.catalog_phase
            && self.catalog_tokens == other.catalog_tokens
            && same_snapshot(&self.catalog, &other.catalog)
    }
    fn rows(&self) -> &[Arc<home_client_model::api_tasks::TaskResource>] {
        self.items
            .as_ref()
            .map_or(&[], |snapshot| match &snapshot.rows {
                CapturedRows::Items { rows, .. } => rows,
                CapturedRows::Catalog(_) => &[],
            })
    }
    /// The raw summary of a row, for the editor draft; the title shown in
    /// rows is the parsed one.
    fn summary(&self, index: usize) -> Option<&str> {
        Some(self.rows().get(index)?.content().summary().as_str())
    }
    /// The nearest top-level row above `index`, which a sub-item there nests under.
    fn parent_token(&self, index: usize) -> Option<i32> {
        (0..index).rev().find_map(|above| {
            let summary = self.summary(above)?;
            (!item_text::parse(summary).nested)
                .then(|| self.item_tokens.get(above).copied())
                .flatten()
        })
    }
    /// "done/total" over the sub-items right under a top-level row; empty without any.
    fn progress(&self, index: usize) -> String {
        let (done, total) = self
            .rows()
            .iter()
            .skip(index + 1)
            .map(|row| row.content())
            .take_while(|content| item_text::parse(content.summary().as_str()).nested)
            .fold((0usize, 0usize), |(done, total), content| {
                (done + usize::from(content.completed()), total + 1)
            });
        if total == 0 {
            String::new()
        } else {
            format!("{done}/{total}")
        }
    }
    fn row(&self, index: usize) -> Option<ProTaskRow> {
        let row = self.rows().get(index)?;
        let key = ItemKey::new(row.key().list().clone(), row.key().uid().clone());
        let draft = self.drafts.get(&key);
        let phase = if self.busy && self.selected.as_ref() == Some(&key) {
            ProTaskPhase::Pending
        } else {
            match draft.map(|draft| draft.phase) {
                None | Some(DraftPhase::Editing) => ProTaskPhase::Ready,
                Some(DraftPhase::Conflict) => ProTaskPhase::Conflict,
                Some(DraftPhase::Unknown) => ProTaskPhase::CommitUnknown,
            }
        };
        let summary = item_text::parse(row.content().summary().as_str());
        let parent = if summary.nested {
            self.parent_token(index)
        } else {
            None
        };
        Some(ProTaskRow {
            token: *self.item_tokens.get(index)?,
            generation: self.generation,
            title: summary.title.to_owned().into(),
            completed: row.content().completed(),
            writable: true,
            detail: row
                .content()
                .description()
                .map(|value| value.as_str())
                .unwrap_or_default()
                .to_owned()
                .into(),
            // A sub-item with no top-level row above it is drawn indented but
            // belongs to no parent: `parent` is read only while `nested`.
            nested: summary.nested,
            parent: parent.unwrap_or_default(),
            quantity: summary
                .quantity
                .map(item_text::quantity_badge)
                .unwrap_or_default()
                .into(),
            progress: if summary.nested {
                String::new()
            } else {
                self.progress(index)
            }
            .into(),
            phase,
            status: match phase {
                ProTaskPhase::Ready => {
                    if draft.is_some() {
                        "Unsaved draft"
                    } else {
                        ""
                    }
                }
                ProTaskPhase::Pending => "",
                ProTaskPhase::Conflict => "Conflict to resolve",
                ProTaskPhase::CommitUnknown => "Outcome unknown · no resubmission",
                ProTaskPhase::CommittedResponseWithheld => "Access removed",
            }
            .into(),
        })
    }
}
fn same_snapshot(a: &Option<Arc<ListReadSnapshot>>, b: &Option<Arc<ListReadSnapshot>>) -> bool {
    match (a, b) {
        (Some(a), Some(b)) => Arc::ptr_eq(a, b),
        (None, None) => true,
        _ => false,
    }
}
struct Tasks {
    view: Arc<Projection>,
    notify: ModelNotify,
}
impl Model for Tasks {
    type Data = ProTaskRow;
    fn row_count(&self) -> usize {
        self.view.rows().len()
    }
    fn row_data(&self, index: usize) -> Option<Self::Data> {
        self.view.row(index)
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
    }
    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}
struct Catalog {
    view: Arc<Projection>,
    notify: ModelNotify,
}
struct GroceryRows {
    view: Arc<Projection>,
    indices: Vec<usize>,
    notify: ModelNotify,
}
impl Model for GroceryRows {
    type Data = ProTaskRow;
    fn row_count(&self) -> usize {
        self.indices.len()
    }
    fn row_data(&self, index: usize) -> Option<Self::Data> {
        self.view.row(*self.indices.get(index)?)
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
    }
    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}
// The items checked off earlier, offered for a quick re-add: activating one
// asks the owner to uncheck it. Nothing here is a frequency model.
struct CommonRows {
    view: Arc<Projection>,
    indices: Vec<usize>,
    notify: ModelNotify,
}
impl Model for CommonRows {
    type Data = ProGrocerySuggestion;
    fn row_count(&self) -> usize {
        self.indices.len()
    }
    fn row_data(&self, index: usize) -> Option<Self::Data> {
        let row = self.view.row(*self.indices.get(index)?)?;
        Some(ProGrocerySuggestion {
            token: row.token,
            generation: row.generation,
            title: row.title,
            detail: row.detail,
            phase: row.phase,
            enabled: row.completed && row.writable && !self.view.busy && self.view.active,
            status: if row.status.is_empty() {
                "Add again".into()
            } else {
                row.status
            },
            glyph: Default::default(),
            present: !row.completed,
        })
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
    }
    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}
fn visible_indices(view: &Projection, shopping_only: bool, limit: usize) -> Vec<usize> {
    view.rows()
        .iter()
        .enumerate()
        .filter(|(_, row)| !shopping_only || !row.content().completed())
        .take(limit)
        .map(|(index, _)| index)
        .collect()
}
/// The rows checked off, in list order: the page's completed group.
fn completed_indices(view: &Projection) -> Vec<usize> {
    view.rows()
        .iter()
        .enumerate()
        .filter(|(_, row)| row.content().completed())
        .map(|(index, _)| index)
        .collect()
}
fn open_count(view: &Projection) -> usize {
    view.rows()
        .iter()
        .filter(|row| !row.content().completed())
        .count()
}
impl Model for Catalog {
    type Data = ContentItem;
    fn row_count(&self) -> usize {
        self.view
            .catalog
            .as_ref()
            .map_or(0, |snapshot| match &snapshot.rows {
                CapturedRows::Catalog(rows) => rows.len(),
                CapturedRows::Items { .. } => 0,
            })
    }
    fn row_data(&self, index: usize) -> Option<Self::Data> {
        let CapturedRows::Catalog(rows) = &self.view.catalog.as_ref()?.rows else {
            return None;
        };
        let row = rows.get(index)?;
        Some(ContentItem {
            token: *self.view.catalog_tokens.get(index)?,
            title: row.display_name().as_str().to_owned().into(),
            link_label: "Open".into(),
            disabled: !self.view.active
                || self.view.busy
                || self.view.catalog_phase != Freshness::Online,
            ..Default::default()
        })
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
    }
    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}
pub(super) fn render(window: &HomewardWindow, projection: Projection) {
    let view = Arc::new(projection);
    let same_list = window.get_lists_list_token() == view.list_token;
    if window.get_lists_privacy_generation() != view.privacy_generation {
        window.set_lists_create_draft("".into());
        window.set_lists_rename_draft("".into());
        window.set_lists_add_draft("".into());
        window.set_lists_privacy_generation(view.privacy_generation);
    }
    if window.get_lists_list_token() != view.list_token {
        window.set_lists_rename_draft("".into());
        window.set_lists_add_draft("".into());
        window.set_lists_list_token(view.list_token);
    }
    if let Some((id, text)) = &view.confirmed_add {
        let id = id.to_string();
        if window.get_lists_confirmed_add_id().as_str() != id {
            // A delayed receipt must not erase text typed after its request.
            if window.get_lists_add_draft().as_str() == text {
                window.set_lists_add_draft("".into());
            }
            window.set_lists_confirmed_add_id(id.into());
        }
    }
    let old_focus = window.get_lists_editor_focused();
    let old_token = window.get_lists_task_selection().token;
    let old_index = window.get_lists_active_index();
    let old_scroll = window.get_lists_scroll_y();
    let selected = view.selected.as_ref().and_then(|key| {
        view.rows()
            .iter()
            .position(|row| row.key().uid() == key.uid() && row.key().list() == key.list())
    });
    let selection = selected
        .and_then(|index| view.row(index))
        .unwrap_or_default();
    window.set_lists_generation(view.generation);
    window.set_lists_catalog_ready(
        view.active && !view.busy && view.catalog_phase == Freshness::Online,
    );
    window.set_lists_write_ready(
        view.active
            && !view.busy
            && view.catalog_phase == Freshness::Online
            && view.item_phase == Freshness::Online,
    );
    window.set_lists_busy(view.busy);
    window.set_lists_catalog_count(
        view.catalog
            .as_ref()
            .map_or_else(String::new, |snapshot| format!("{} lists", snapshot.count))
            .into(),
    );
    let open = open_count(&view);
    window.set_lists_item_count(
        view.items
            .as_ref()
            .map_or_else(String::new, |_| match open {
                0 => "Nothing to buy".to_owned(),
                1 => "1 to buy".to_owned(),
                count => format!("{count} to buy"),
            })
            .into(),
    );
    window.set_lists_selected(view.items.is_some());
    window.set_lists_title(
        view.items
            .as_ref()
            .and_then(|snapshot| match &snapshot.metadata {
                ListReadMetadata::Catalog => None,
                ListReadMetadata::List(list) => Some(list.display_name().as_str().to_owned()),
            })
            .unwrap_or_else(|| "Lists".into())
            .into(),
    );
    let access =
        if view.catalog_phase == Freshness::Offline || view.item_phase == Freshness::Offline {
            ProTaskAccess::Offline
        } else if view.active
            && !view.busy
            && view.catalog_phase == Freshness::Online
            && view.item_phase == Freshness::Online
        {
            ProTaskAccess::Editable
        } else {
            ProTaskAccess::ReadOnly
        };
    window.set_lists_access(access);
    let cached = view
        .items
        .as_ref()
        .or(view.catalog.as_ref())
        .map(|snapshot| {
            chrono::DateTime::<chrono::Utc>::from(snapshot.fetched_at)
                .format("%Y-%m-%d %H:%M:%S UTC")
                .to_string()
        })
        .unwrap_or_default();
    window.set_lists_cached_at(cached.into());
    window.set_lists_notice(view.notice.clone().into());
    let catalog = window.get_lists_catalog();
    if !catalog
        .as_any()
        .downcast_ref::<Catalog>()
        .is_some_and(|old| old.view.same_catalog(&view))
    {
        window.set_lists_catalog(ModelRc::new(Catalog {
            view: view.clone(),
            notify: ModelNotify::default(),
        }));
    }
    let options = view
        .catalog
        .as_ref()
        .map_or_else(Vec::new, |snapshot| match &snapshot.rows {
            CapturedRows::Catalog(rows) => rows
                .iter()
                .enumerate()
                .filter_map(|(index, row)| {
                    let token = *view.catalog_tokens.get(index)?;
                    Some(OptionRow {
                        token,
                        generation: view.generation,
                        label: row.display_name().as_str().to_owned().into(),
                        // The list tab's count badge: only the selected list's
                        // items are read, so the other tabs carry none.
                        description: if token == view.list_token && view.items.is_some() {
                            open.to_string()
                        } else {
                            String::new()
                        }
                        .into(),
                        disabled: !view.active
                            || view.busy
                            || view.catalog_phase != Freshness::Online,
                    })
                })
                .collect(),
            CapturedRows::Items { .. } => Vec::new(),
        });
    let previous_options = window.get_lists_options();
    if !previous_options.iter().eq(options.iter().cloned()) {
        window.set_lists_options(ModelRc::new(slint::VecModel::from(options)));
    }
    window.set_lists_option_index(
        view.catalog_tokens
            .iter()
            .position(|token| *token == view.list_token)
            .map_or(-1, |index| index as i32),
    );
    let grocery = window.get_lists_grocery_rows();
    if !grocery
        .as_any()
        .downcast_ref::<GroceryRows>()
        .is_some_and(|old| old.view.same_tasks(&view))
    {
        window.set_lists_grocery_rows(ModelRc::new(GroceryRows {
            indices: visible_indices(&view, true, usize::MAX),
            view: view.clone(),
            notify: ModelNotify::default(),
        }));
    }
    let completed = window.get_lists_completed_rows();
    if !completed
        .as_any()
        .downcast_ref::<GroceryRows>()
        .is_some_and(|old| old.view.same_tasks(&view))
    {
        window.set_lists_completed_rows(ModelRc::new(GroceryRows {
            indices: completed_indices(&view),
            view: view.clone(),
            notify: ModelNotify::default(),
        }));
    }
    let suggestions = window.get_lists_suggestions();
    if !suggestions
        .as_any()
        .downcast_ref::<CommonRows>()
        .is_some_and(|old| old.view.active == view.active && old.view.same_tasks(&view))
    {
        window.set_lists_suggestions(ModelRc::new(CommonRows {
            indices: completed_indices(&view),
            view: view.clone(),
            notify: ModelNotify::default(),
        }));
    }
    let tasks = window.get_lists_tasks();
    if !tasks
        .as_any()
        .downcast_ref::<Tasks>()
        .is_some_and(|old| old.view.same_tasks(&view))
    {
        window.set_lists_tasks(ModelRc::new(Tasks {
            view: view.clone(),
            notify: ModelNotify::default(),
        }));
    }
    window.set_lists_task_selected(selected.is_some());
    window.set_lists_active_index(selected.map_or(-1, |index| index as i32));
    window.set_lists_task_selection(selection.clone());
    // The editor edits the raw summary, so a saved draft keeps the nesting
    // marker and the quantity word that the row's title leaves out.
    let draft = view
        .selected
        .as_ref()
        .and_then(|key| view.drafts.get(key))
        .map(|draft| draft.text.clone())
        .or_else(|| selected.and_then(|index| view.summary(index).map(str::to_owned)))
        .unwrap_or_default();
    window.set_lists_draft(draft.into());
    // A newly selected or moved row must be revealed by the task component.
    // Preserve the viewport only when the presentation target stayed put.
    if same_list
        && selection.token == old_token
        && selected.map_or(-1, |index| index as i32) == old_index
    {
        window.invoke_lists_scroll_to(old_scroll);
    }
    if old_focus && selection.token == old_token && selected.is_some() {
        window.invoke_lists_focus_editor();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use slint::ComponentHandle;

    /// The list tab that announces itself checked under the selected list's name.
    fn selected_tab(window: &HomewardWindow) -> Option<i_slint_backend_testing::ElementHandle> {
        use i_slint_backend_testing::ElementQuery;
        let index = window.get_lists_option_index();
        let label = usize::try_from(index)
            .ok()
            .and_then(|index| window.get_lists_options().row_data(index))?
            .label;
        ElementQuery::from_root(window)
            .match_predicate(move |e| {
                e.accessible_checkable() == Some(true)
                    && e.accessible_checked() == Some(true)
                    && e.accessible_label().as_deref() == Some(label.as_str())
            })
            .find_first()
    }

    #[test]
    fn grocery_inspector_preserves_list_context_and_drafts_across_sizes() {
        use i_slint_backend_testing::{ElementQuery, TestingBackend, TestingBackendOptions};
        use slint::{
            LogicalSize,
            platform::{Key, PointerEventButton, WindowEvent},
        };
        use std::{cell::RefCell, rc::Rc};
        slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
            mock_time: true,
            renderer_name: Some("software".into()),
            ..Default::default()
        })))
        .unwrap();
        let directory =
            std::env::var_os("HOMEWARD_SYNTHETIC_RENDER_DIR").map(std::path::PathBuf::from);
        if let Some(path) = &directory {
            std::fs::create_dir_all(path).unwrap();
        }
        for large in [false, true] {
            for dark in [false, true] {
                for width in [390., 1600.] {
                    let window = crate::create_window().unwrap();
                    window.set_access_stage(crate::ProAccessStage::Connected);
                    window.set_endpoint_ready(true);
                    window.set_show_endpoint(false);
                    window.set_page(crate::ViewPage::Lists);
                    window.set_dark_mode(dark);
                    window.set_large_text(large);
                    window.window().set_size(LogicalSize::new(width, 1000.));
                    let mut state = crate::list_state::tests::ready();
                    let names: Vec<_> = (0..160)
                        .map(|i| (format!("item-{i}"), format!("Synthetic item {i}")))
                        .collect();
                    let values: Vec<_> = names
                        .iter()
                        .map(|(id, name)| (id.as_str(), 1, name.as_str()))
                        .collect();
                    state
                        .publish_items(crate::list_state::tests::items(
                            crate::list_state::tests::LIST,
                            11,
                            &values,
                        ))
                        .unwrap();
                    let state = Rc::new(RefCell::new(state));
                    render(&window, Projection::new(&state.borrow()));
                    let weak = window.as_weak();
                    let owner = state.clone();
                    window.on_lists_grocery_select(move |_, _, t, g| {
                        let mut owner = owner.borrow_mut();
                        owner.select_item(t, g).unwrap();
                        render(&weak.upgrade().unwrap(), Projection::new(&owner));
                    });
                    window.on_lists_action(|_, _, _| panic!("navigation must not write"));
                    window.on_lists_grocery_complete(|_, _, _, _, _| {
                        panic!("navigation must not check an item")
                    });
                    window.set_lists_add_draft("Unsent addition".into());
                    window.show().unwrap();
                    window.invoke_lists_scroll_to(-1200.);
                    slint::platform::update_timers_and_animations();
                    let before = window.get_lists_scroll_y();
                    let page = ElementQuery::from_root(&window)
                        .match_id("HomewardLists::page")
                        .find_first()
                        .unwrap();
                    let top = page.absolute_position().y;
                    let bottom = top + page.size().height;
                    let edit = ElementQuery::from_root(&window)
                        .match_inherits("IdentityButton")
                        .match_predicate(move |e| {
                            e.absolute_position().y >= top
                                && e.absolute_position().y + e.size().height <= bottom
                        })
                        .match_predicate(|e| {
                            e.accessible_label().is_some_and(|s| {
                                s.starts_with("Synthetic item") && s.ends_with(" · Edit")
                            })
                        })
                        .find_first()
                        .expect("visible grocery item edit");
                    edit.mock_single_click(PointerEventButton::Left);
                    slint::platform::update_timers_and_animations();
                    let inspector = ElementQuery::from_root(&window)
                        .match_id("HomewardLists::inspector")
                        .find_first()
                        .unwrap();
                    let close_label = if width >= 1040. && !large {
                        "Done Editing"
                    } else {
                        "Back to List"
                    };
                    let close = ElementQuery::from_root(&window)
                        .match_inherits("IdentityButton")
                        .match_predicate(|e| e.accessible_label().as_deref() == Some(close_label))
                        .find_first()
                        .unwrap();
                    assert!(
                        close.absolute_position().y < 250.,
                        "editor opens at the viewport, not after 160 items"
                    );
                    assert!(inspector.size().width <= width);
                    if close_label == "Done Editing" {
                        assert!(
                            page.absolute_position().x + page.size().width
                                <= inspector.absolute_position().x,
                            "list and inspector must not overlap"
                        );
                    }
                    if close_label == "Back to List" {
                        assert!(
                            selected_tab(&window).is_none(),
                            "narrow background must be inaccessible"
                        );
                    }
                    window.set_lists_draft("Retained edit".into());
                    if let Some(path) = &directory {
                        let pixels = window.window().take_snapshot().unwrap();
                        let name = format!(
                            "grocery-{width}-{}-{}.png",
                            if dark { "dark" } else { "light" },
                            if large { 200 } else { 100 }
                        );
                        let mut encoder = png::Encoder::new(
                            std::fs::File::create(path.join(name)).unwrap(),
                            pixels.width(),
                            pixels.height(),
                        );
                        encoder.set_color(png::ColorType::Rgba);
                        encoder.set_depth(png::BitDepth::Eight);
                        encoder
                            .write_header()
                            .unwrap()
                            .write_image_data(pixels.as_bytes())
                            .unwrap();
                    }
                    // Resize with the same editor and keyed draft still alive.
                    window.window().set_size(LogicalSize::new(800., 1000.));
                    slint::platform::update_timers_and_animations();
                    window.window().dispatch_event(WindowEvent::KeyPressed {
                        text: Key::Escape.into(),
                    });
                    window.window().dispatch_event(WindowEvent::KeyReleased {
                        text: Key::Escape.into(),
                    });
                    slint::platform::update_timers_and_animations();
                    assert!(
                        ElementQuery::from_root(&window)
                            .match_id("HomewardLists::inspector")
                            .find_first()
                            .is_none()
                    );
                    assert_eq!(window.get_lists_draft().as_str(), "Retained edit");
                    assert_eq!(window.get_lists_add_draft().as_str(), "Unsent addition");
                    assert!(
                        (window.get_lists_scroll_y() - before).abs() < 1.,
                        "list position survives editing and resizing"
                    );
                    assert!(
                        selected_tab(&window).is_some(),
                        "the list tabs return with the list"
                    );
                    // At 200 % text the header's longest title word already
                    // fills a phone-wide list card and pushes the trailing
                    // options action past the card edge (ProGroceries wraps
                    // neither its title nor that action onto a second row),
                    // so the menu flow is exercised at 100 % text.
                    if !large && width == 390. {
                        window.window().set_size(LogicalSize::new(390., 700.));
                        // The options action lives in the list card header, at the top of the page.
                        window.invoke_lists_scroll_to(0.);
                        slint::platform::update_timers_and_animations();
                        let options = ElementQuery::from_root(&window)
                            .match_accessible_role(i_slint_backend_testing::AccessibleRole::Button)
                            .match_predicate(|e| {
                                e.accessible_label().as_deref() == Some("List options")
                            })
                            .find_first()
                            .expect("the list header exposes its options action");
                        options.mock_single_click(PointerEventButton::Left);
                        slint::platform::update_timers_and_animations();
                        let create = ElementQuery::from_root(&window)
                            .match_inherits("KitOption")
                            .match_predicate(|e| {
                                e.accessible_label().as_deref() == Some("New list")
                            })
                            .find_first()
                            .expect("the options menu lists the new-list action");
                        create.mock_single_click(PointerEventButton::Left);
                        slint::platform::update_timers_and_animations();
                        assert!(
                            ElementQuery::from_root(&window)
                                .match_inherits("KitOption")
                                .find_first()
                                .is_none(),
                            "an activated menu closes"
                        );
                        let management = ElementQuery::from_root(&window)
                            .match_inherits("TextInput")
                            .match_predicate(|e| {
                                e.accessible_label().as_deref() == Some("New List")
                            })
                            .find_first()
                            .unwrap();
                        let manager_top = management.absolute_position().y;
                        let tabs_top = selected_tab(&window).unwrap().absolute_position().y;
                        assert!(manager_top >= page.absolute_position().y);
                        window.invoke_lists_scroll_to(-200.);
                        slint::platform::update_timers_and_animations();
                        assert!(
                            management.absolute_position().y < manager_top - 100.,
                            "management fields belong to the page scroll"
                        );
                        assert_eq!(
                            selected_tab(&window).unwrap().absolute_position().y,
                            tabs_top,
                            "the list tabs stay available"
                        );
                    }
                    window.hide().unwrap();
                }
            }
        }
    }

    #[test]
    fn inspector_waits_for_requested_identity_and_never_reveals_refused_selection() {
        use i_slint_backend_testing::ElementQuery;
        use slint::{LogicalSize, platform::PointerEventButton};
        use std::{cell::RefCell, rc::Rc};
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_endpoint_ready(true);
        window.set_show_endpoint(false);
        window.set_page(crate::ViewPage::Lists);
        window.window().set_size(LogicalSize::new(1600., 1000.));
        let mut state = crate::list_state::tests::ready();
        let a = state.item_tokens[0];
        let b = state.item_tokens[1];
        state
            .edit(a, state.generation, "Draft for A".into())
            .unwrap();
        render(&window, Projection::new(&state));
        window.set_lists_add_draft("Unsent addition".into());
        let requests = Rc::new(RefCell::new(Vec::new()));
        let capture = requests.clone();
        // Deliberately withhold the acknowledgement, as the real mailbox does.
        window
            .on_lists_grocery_select(move |lt, lg, t, g| capture.borrow_mut().push((lt, lg, t, g)));
        window.on_lists_action(|_, _, _| panic!("selection must not mutate"));
        window.show().unwrap();
        slint::platform::update_timers_and_animations();
        let open = |title: &str| {
            let label = format!("{title} · Edit");
            ElementQuery::from_root(&window)
                .match_inherits("IdentityButton")
                .match_predicate(move |e| e.accessible_label().as_deref() == Some(label.as_str()))
                .find_first()
                .unwrap()
                .mock_single_click(PointerEventButton::Left);
            slint::platform::update_timers_and_animations();
        };
        let hidden = || {
            assert!(
                ElementQuery::from_root(&window)
                    .match_id("HomewardLists::inspector")
                    .find_first()
                    .is_none()
            );
            assert!(
                ElementQuery::from_root(&window)
                    .match_inherits("TextInput")
                    .match_predicate(|e| e.accessible_label().as_deref() == Some("Item"))
                    .find_first()
                    .is_none()
            );
        };
        open("Server two");
        assert_eq!(requests.borrow().last().unwrap().2, b);
        hidden();
        assert_eq!(window.get_lists_draft().as_str(), "Draft for A");
        let request_generation = state.generation;
        state.select_item(b, request_generation).unwrap();
        assert_ne!(state.generation, request_generation);
        render(&window, Projection::new(&state));
        slint::platform::update_timers_and_animations();
        assert!(
            ElementQuery::from_root(&window)
                .match_id("HomewardLists::inspector")
                .find_first()
                .is_some()
        );
        assert_eq!(window.get_lists_task_selection().token, b);
        assert_eq!(window.get_lists_draft().as_str(), "Server two");
        assert!(window.get_lists_editor_focused());
        assert!(window.invoke_dismiss_selection_popover());
        slint::platform::update_timers_and_animations();
        open("Server one");
        state.notice = "Selection refused";
        render(&window, Projection::new(&state));
        slint::platform::update_timers_and_animations();
        hidden();
        assert!(window.invoke_dismiss_selection_popover());
        // An acknowledgement after dismissal must not reopen the inspector.
        state.select_item(a, state.generation).unwrap();
        render(&window, Projection::new(&state));
        slint::platform::update_timers_and_animations();
        hidden();
        assert_eq!(window.get_lists_draft().as_str(), "Draft for A");
        assert_eq!(window.get_lists_add_draft().as_str(), "Unsent addition");
        // A privacy reset cancels a pending target even if that target is later
        // published by an already queued presentation.
        open("Server two");
        window.set_lists_privacy_generation(window.get_lists_privacy_generation() + 1);
        slint::platform::update_timers_and_animations();
        state.select_item(b, state.generation).unwrap();
        render(&window, Projection::new(&state));
        slint::platform::update_timers_and_animations();
        hidden();
        window.hide().unwrap();
    }

    #[test]
    fn the_selected_list_is_the_checked_tab_and_clearing_removes_every_tab() {
        use i_slint_backend_testing::ElementQuery;
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_endpoint_ready(true);
        window.set_show_endpoint(false);
        window.set_page(crate::ViewPage::Lists);
        let mut state = crate::list_state::tests::ready();
        render(&window, Projection::new(&state));
        window.show().unwrap();
        let expected = window
            .get_lists_options()
            .row_data(window.get_lists_option_index() as usize)
            .unwrap()
            .label;
        let tab = selected_tab(&window).expect("the selected list is the checked tab");
        assert_eq!(tab.accessible_label().as_deref(), Some(expected.as_str()));
        assert_eq!(
            tab.accessible_description().as_deref(),
            Some("2 to buy"),
            "only the read list carries its open count"
        );
        assert_eq!(window.get_lists_item_count().as_str(), "2 to buy");
        state.clear();
        render(&window, Projection::new(&state));
        assert!(selected_tab(&window).is_none());
        assert!(
            ElementQuery::from_root(&window)
                .match_predicate(|e| e.accessible_checkable() == Some(true)
                    && e.accessible_label()
                        .is_some_and(|label| label.starts_with("Synthetic")))
                .find_first()
                .is_none(),
            "no catalogue, no tabs"
        );
        window.hide().unwrap();
    }

    #[test]
    fn list_tabs_activate_by_fenced_identity_and_disabled_tabs_stay_inert() {
        use i_slint_backend_testing::ElementQuery;
        use slint::platform::PointerEventButton;
        use std::{cell::RefCell, rc::Rc};
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_endpoint_ready(true);
        window.set_show_endpoint(false);
        window.set_page(crate::ViewPage::Lists);
        window.set_lists_catalog_ready(true);
        window.set_lists_generation(7);
        window.set_lists_list_token(22);
        window.set_lists_option_index(1);
        let rows = |generation, gamma_disabled| {
            ModelRc::from([
                OptionRow {
                    token: 11,
                    generation,
                    label: "Alpha store".into(),
                    ..Default::default()
                },
                OptionRow {
                    token: 22,
                    generation,
                    label: "Beta store".into(),
                    description: "4".into(),
                    ..Default::default()
                },
                OptionRow {
                    token: 33,
                    generation,
                    label: "Gamma store".into(),
                    disabled: gamma_disabled,
                    ..Default::default()
                },
            ])
        };
        window.set_lists_options(rows(7, true));
        let calls = Rc::new(RefCell::new(Vec::new()));
        let out = calls.clone();
        window.on_lists_choose(move |t, g| out.borrow_mut().push((t, g)));
        window.show().unwrap();
        let tab = |label: &str| {
            let label = label.to_owned();
            ElementQuery::from_root(&window)
                .match_predicate(move |e| {
                    e.accessible_checkable() == Some(true)
                        && e.accessible_label().as_deref() == Some(label.as_str())
                })
                .find_first()
                .unwrap()
        };
        let press = |label: &str| {
            tab(label).mock_single_click(PointerEventButton::Left);
            slint::platform::update_timers_and_animations();
        };
        assert_eq!(tab("Beta store").accessible_checked(), Some(true));
        assert_eq!(
            tab("Beta store").accessible_description().as_deref(),
            Some("4 to buy")
        );
        assert_eq!(tab("Alpha store").accessible_checked(), Some(false));
        press("Alpha store");
        assert_eq!(*calls.borrow(), [(11, 7)]);
        assert_eq!(
            window.get_lists_list_token(),
            22,
            "a tab asks the owner; it cannot select a list itself"
        );
        press("Beta store");
        assert_eq!(
            calls.borrow().len(),
            1,
            "the selected tab does not request its own list again"
        );
        press("Gamma store");
        assert_eq!(calls.borrow().len(), 1, "a disabled tab is inert");
        window.set_lists_generation(8);
        window.set_lists_options(rows(8, false));
        slint::platform::update_timers_and_animations();
        press("Gamma store");
        assert_eq!(
            *calls.borrow(),
            [(11, 7), (33, 8)],
            "a press reports the identity it started on"
        );
        window.hide().unwrap();
    }

    #[test]
    fn an_add_confirmation_only_clears_the_exact_submitted_text_once() {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        let mut state = crate::list_state::tests::ready();
        render(&window, Projection::new(&state));
        window.set_lists_add_draft("Newer unsent text".into());
        state.confirmed_add = Some((1, "Submitted text".into()));
        render(&window, Projection::new(&state));
        assert_eq!(window.get_lists_add_draft().as_str(), "Newer unsent text");
        // A later refresh of the same receipt must not clear a newly typed
        // identical value either: each receipt is consumed once.
        window.set_lists_add_draft("Submitted text".into());
        render(&window, Projection::new(&state));
        assert_eq!(window.get_lists_add_draft().as_str(), "Submitted text");
        state.confirmed_add = Some((2, "Submitted text".into()));
        render(&window, Projection::new(&state));
        assert!(window.get_lists_add_draft().is_empty());
        window.hide().unwrap();
    }

    #[test]
    fn a_coalesced_active_projection_still_erases_all_header_drafts_after_a_private_reset() {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        let mut state = crate::list_state::tests::ready();
        render(&window, Projection::new(&state));
        window.set_lists_create_draft("Private create draft".into());
        window.set_lists_rename_draft("Private rename draft".into());
        window.set_lists_add_draft("Private task draft".into());
        state.clear();
        // The empty reset may coalesce with a fresh authorized capture before
        // the event loop renders. Deliver only that final active projection.
        state.active = true;
        state
            .publish_catalog(crate::list_state::tests::catalog(&[
                crate::list_state::tests::LIST,
            ]))
            .unwrap();
        render(&window, Projection::new(&state));
        assert!(window.get_lists_create_draft().is_empty());
        assert!(window.get_lists_rename_draft().is_empty());
        assert!(window.get_lists_add_draft().is_empty());
        assert!(window.get_lists_draft().is_empty());
        assert_eq!(window.get_lists_tasks().row_count(), 0);
        window.hide().unwrap();
    }

    #[test]
    fn changing_the_selected_list_drops_header_values_without_losing_a_keyed_task_draft() {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        let mut state = crate::list_state::tests::ready();
        state
            .edit(
                state.item_tokens[0],
                state.generation,
                "Retained keyed task draft".into(),
            )
            .unwrap();
        render(&window, Projection::new(&state));
        window.set_lists_rename_draft("Name for the old list".into());
        window.set_lists_add_draft("Item for the old list".into());
        state
            .select_list(state.catalog_tokens[1], state.generation)
            .unwrap();
        render(&window, Projection::new(&state));
        assert!(window.get_lists_rename_draft().is_empty());
        assert!(window.get_lists_add_draft().is_empty());
        assert_eq!(state.drafts.len(), 1);
    }
}

/// A dedicated list owner publishes only into its captured dashboard widget.
/// The page catalogue, selection, draft and focus models remain independent.
pub(super) fn render_widget(
    window: &HomewardWindow,
    mut projection: Projection,
    target: &crate::dashboard_widget_host::TaskTarget,
    source: Option<(
        home_client_session::GenerationGuard,
        Option<home_client_session::Generation>,
    )>,
) {
    if source.as_ref().is_none_or(|(session, generation)| {
        generation.is_none_or(|generation| !session.is_current(generation))
    }) {
        projection.active = false;
    }
    let ready = projection.active
        && projection.item_phase == Freshness::Online
        && projection.catalog_phase == Freshness::Online
        && projection.items.as_ref().is_some_and(
            |snapshot| matches!(&snapshot.rows,CapturedRows::Items{list,..} if list==target.list.native_id()),
        );
    let status = if !projection.notice.is_empty() {
        projection.notice.clone()
    } else if !projection.active {
        "Source unavailable".into()
    } else {
        match projection.item_phase {
            Freshness::Empty | Freshness::Syncing => "Reading list…".into(),
            Freshness::Offline => "Offline · dated copy · changes suspended".into(),
            Freshness::Online => String::new(),
        }
    };
    let access = if !ready {
        ProTaskAccess::Offline
    } else if projection.busy {
        ProTaskAccess::ReadOnly
    } else {
        ProTaskAccess::Editable
    };
    let view = Arc::new(projection);
    let previous = crate::cache_view::widget_task_rows(window, target.token, target.generation);
    let rows = if previous
        .as_any()
        .downcast_ref::<WidgetTasks>()
        .is_some_and(|old| old.view.active == view.active && old.view.same_tasks(&view))
    {
        previous
    } else {
        ModelRc::new(WidgetTasks {
            indices: visible_indices(&view, target.shopping_only, target.limit),
            view,
            notify: ModelNotify::default(),
        })
    };
    crate::cache_view::publish_widget_tasks(
        window,
        target.token,
        target.generation,
        rows,
        access,
        ready,
        status,
    );
}
struct WidgetTasks {
    view: Arc<Projection>,
    indices: Vec<usize>,
    notify: ModelNotify,
}
impl Model for WidgetTasks {
    type Data = ProTaskRow;
    fn row_count(&self) -> usize {
        if self.view.active {
            self.indices.len()
        } else {
            0
        }
    }
    fn row_data(&self, index: usize) -> Option<Self::Data> {
        if index >= self.row_count() {
            return None;
        };
        self.view.row(*self.indices.get(index)?)
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
    }
    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}

#[cfg(test)]
mod widget_tests {
    use super::*;
    #[test]
    fn dashboard_preview_limit_is_a_model_boundary_and_does_not_rewrite_observed_rows() {
        let state = crate::list_state::tests::ready();
        let rows = WidgetTasks {
            view: Arc::new(Projection::new(&state)),
            indices: vec![0],
            notify: ModelNotify::default(),
        };
        assert_eq!(rows.row_count(), 1);
        assert!(rows.row_data(1).is_none());
        let row = rows.row_data(0).unwrap();
        assert_eq!(row.title.as_str(), "Server one");
        assert!(!row.completed);
        let mut cleared = state;
        cleared.clear();
        let rows = WidgetTasks {
            view: Arc::new(Projection::new(&cleared)),
            indices: vec![],
            notify: ModelNotify::default(),
        };
        assert_eq!(rows.row_count(), 0);
        assert!(rows.row_data(0).is_none());
    }

    #[test]
    fn shopping_filters_keep_original_tokens_and_common_items_keep_completed_uids() {
        use home_client_cache::lists::CapturedRows;
        let mut state = crate::list_state::tests::ready();
        let snapshot = crate::list_state::tests::items(
            crate::list_state::tests::LIST,
            11,
            &[
                ("done-a", 1, "Completed A"),
                ("active-b", 1, "Active B"),
                ("done-c", 1, "Completed C"),
                ("active-d", 1, "Active D"),
            ],
        );
        let mut snapshot = Arc::try_unwrap(snapshot).ok().unwrap();
        if let CapturedRows::Items { rows, .. } = &mut snapshot.rows {
            rows[0] = Arc::new(
                rows[0]
                    .with_content(rows[0].content().with_completed(true))
                    .unwrap(),
            );
            rows[2] = Arc::new(
                rows[2]
                    .with_content(rows[2].content().with_completed(true))
                    .unwrap(),
            );
        }
        state.publish_items(Arc::new(snapshot)).unwrap();
        let view = Arc::new(Projection::new(&state));
        let visible = visible_indices(&view, true, 1);
        assert_eq!(
            visible,
            vec![1],
            "filter before limit, preserving the source index"
        );
        let tasks = WidgetTasks {
            view: view.clone(),
            indices: visible,
            notify: ModelNotify::default(),
        };
        assert_eq!(tasks.row_data(0).unwrap().token, state.item_tokens[1]);
        assert_eq!(tasks.row_data(0).unwrap().title.as_str(), "Active B");
        assert_eq!(completed_indices(&view), vec![0, 2]);
        assert_eq!(open_count(&view), 2);
        let common = CommonRows {
            indices: completed_indices(&view),
            view,
            notify: ModelNotify::default(),
        };
        assert_eq!(common.row_count(), 2, "only checked rows are offered again");
        assert_eq!(common.row_data(0).unwrap().token, state.item_tokens[0]);
        assert!(common.row_data(0).unwrap().enabled);
        assert!(!common.row_data(0).unwrap().present);
        assert_eq!(common.row_data(0).unwrap().status.as_str(), "Add again");
        assert_eq!(common.row_data(1).unwrap().token, state.item_tokens[2]);
        assert_eq!(
            state.items.as_ref().unwrap().count,
            4,
            "filtering never rewrites server count"
        );
    }

    #[test]
    fn sub_items_quantities_and_progress_are_parsed_from_the_summary_once() {
        use home_client_cache::lists::CapturedRows;
        let mut state = crate::list_state::tests::ready();
        let snapshot = crate::list_state::tests::items(
            crate::list_state::tests::LIST,
            11,
            &[
                ("flour", 1, "Flour x2"),
                ("yeast", 1, "- Instant yeast"),
                ("sugar", 1, "- Sugar ×3"),
                ("eggs", 1, "Eggs"),
                ("orphan", 1, "- Orphan"),
            ],
        );
        let mut snapshot = Arc::try_unwrap(snapshot).ok().unwrap();
        if let CapturedRows::Items { rows, .. } = &mut snapshot.rows {
            rows[2] = Arc::new(
                rows[2]
                    .with_content(rows[2].content().with_completed(true))
                    .unwrap(),
            );
        }
        // The orphan sits first so no top-level row precedes it.
        if let CapturedRows::Items { rows, .. } = &mut snapshot.rows {
            rows.rotate_right(1);
        }
        state.publish_items(Arc::new(snapshot)).unwrap();
        let view = Projection::new(&state);
        let orphan = view.row(0).unwrap();
        assert!(orphan.nested);
        assert_eq!(
            orphan.parent, 0,
            "a sub-item without a row above has no parent"
        );
        assert_eq!(orphan.title.as_str(), "Orphan");
        let flour = view.row(1).unwrap();
        assert!(!flour.nested);
        assert_eq!(flour.title.as_str(), "Flour");
        assert_eq!(flour.quantity.as_str(), "×2");
        assert_eq!(flour.progress.as_str(), "1/2");
        let yeast = view.row(2).unwrap();
        assert!(yeast.nested);
        assert_eq!(yeast.parent, flour.token);
        assert_eq!(yeast.quantity.as_str(), "");
        assert_eq!(yeast.progress.as_str(), "");
        let sugar = view.row(3).unwrap();
        assert_eq!(sugar.parent, flour.token);
        assert_eq!(sugar.quantity.as_str(), "×3");
        assert!(sugar.completed);
        let eggs = view.row(4).unwrap();
        assert!(!eggs.nested);
        assert_eq!(eggs.progress.as_str(), "");
        assert_eq!(
            view.summary(1),
            Some("Flour x2"),
            "the editor keeps the raw text"
        );
    }
}

#[cfg(test)]
mod stability_tests {
    use super::*;
    #[test]
    fn task_models_survive_unchanged_publication_and_local_draft_text_edits() {
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        let mut state = crate::list_state::tests::ready();
        render(&window, Projection::new(&state));
        let tasks = window.get_lists_tasks();
        let grocery = window.get_lists_grocery_rows();
        let catalog = window.get_lists_catalog();
        let options = window.get_lists_options();
        for _ in 0..20 {
            render(&window, Projection::new(&state));
        }
        assert!(tasks == window.get_lists_tasks());
        assert!(grocery == window.get_lists_grocery_rows());
        assert!(catalog == window.get_lists_catalog());
        assert!(options == window.get_lists_options());
        state
            .edit(state.item_tokens[0], state.generation, "Draft one".into())
            .unwrap();
        render(&window, Projection::new(&state));
        let draft_tasks = window.get_lists_tasks();
        let scroll = window.get_lists_scroll_y();
        state
            .edit(state.item_tokens[0], state.generation, "Draft two".into())
            .unwrap();
        render(&window, Projection::new(&state));
        assert!(draft_tasks == window.get_lists_tasks());
        assert_eq!(window.get_lists_draft(), "Draft two");
        assert_eq!(window.get_lists_scroll_y(), scroll);
        let projection = Projection::new(&state);
        assert!(projection.same(&Projection::new(&state)));
        state.interactive = false;
        assert!(!projection.same(&Projection::new(&state)));
        render(&window, Projection::new(&state));
        assert!(
            draft_tasks == window.get_lists_tasks(),
            "hiding a page retains row identity"
        );
        assert!(!window.get_lists_write_ready());
    }
}
