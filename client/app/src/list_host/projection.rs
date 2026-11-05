use crate::{
    ContentItem, HomewardWindow, OptionRow, ProGrocerySuggestion, ProTaskAccess, ProTaskPhase,
    ProTaskRow,
    list_state::{Draft, DraftPhase, Freshness, State},
};
use home_client_cache::lists::{CapturedRows, ListReadMetadata, ListReadSnapshot};
use home_client_model::lists::ItemKey;
use slint::{Model, ModelNotify, ModelRc, ModelTracker};
use std::{collections::BTreeMap, sync::Arc};

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
            active: state.active && state.foreground,
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
    fn rows(&self) -> &[Arc<home_client_model::api_tasks::TaskResource>] {
        self.items
            .as_ref()
            .map_or(&[], |snapshot| match &snapshot.rows {
                CapturedRows::Items { rows, .. } => rows,
                CapturedRows::Catalog(_) => &[],
            })
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
        Some(ProTaskRow {
            token: *self.item_tokens.get(index)?,
            generation: self.generation,
            title: row.content().summary().as_str().to_owned().into(),
            completed: row.content().completed(),
            writable: true,
            detail: row
                .content()
                .description()
                .map(|value| value.as_str())
                .unwrap_or_default()
                .to_owned()
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
                ProTaskPhase::Pending => "Waiting for the server",
                ProTaskPhase::Conflict => "Conflict to resolve",
                ProTaskPhase::CommitUnknown => "Outcome unknown · no resubmission",
                ProTaskPhase::CommittedResponseWithheld => "Access removed",
            }
            .into(),
        })
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
}
struct CommonRows {
    view: Arc<Projection>,
    notify: ModelNotify,
}
impl Model for CommonRows {
    type Data = ProGrocerySuggestion;
    fn row_count(&self) -> usize {
        self.view.rows().len()
    }
    fn row_data(&self, index: usize) -> Option<Self::Data> {
        let row = self.view.row(index)?;
        Some(ProGrocerySuggestion {
            token: row.token,
            generation: row.generation,
            title: row.title,
            detail: row.detail,
            phase: row.phase,
            enabled: row.completed && row.writable && !self.view.busy && self.view.active,
            status: if row.status.is_empty() {
                if row.completed {
                    "Add again"
                } else {
                    "Already on this list"
                }
                .into()
            } else {
                row.status
            },
        })
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
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
    window.set_lists_item_count(
        view.items
            .as_ref()
            .map_or_else(String::new, |snapshot| {
                format!(
                    "Remaining: {} · Saved: {}",
                    view.rows()
                        .iter()
                        .filter(|row| !row.content().completed())
                        .count(),
                    snapshot.count
                )
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
    window.set_lists_catalog(ModelRc::new(Catalog {
        view: view.clone(),
        notify: ModelNotify::default(),
    }));
    let options = view
        .catalog
        .as_ref()
        .map_or_else(Vec::new, |snapshot| match &snapshot.rows {
            CapturedRows::Catalog(rows) => rows
                .iter()
                .enumerate()
                .filter_map(|(index, row)| {
                    Some(OptionRow {
                        token: *view.catalog_tokens.get(index)?,
                        generation: view.generation,
                        label: row.display_name().as_str().to_owned().into(),
                        description: "".into(),
                        disabled: !view.active
                            || view.busy
                            || view.catalog_phase != Freshness::Online,
                    })
                })
                .collect(),
            CapturedRows::Items { .. } => Vec::new(),
        });
    window.set_lists_options(ModelRc::new(slint::VecModel::from(options)));
    window.set_lists_option_index(
        view.catalog_tokens
            .iter()
            .position(|token| *token == view.list_token)
            .map_or(-1, |index| index as i32),
    );
    window.set_lists_grocery_rows(ModelRc::new(GroceryRows {
        indices: visible_indices(&view, true, usize::MAX),
        view: view.clone(),
        notify: ModelNotify::default(),
    }));
    window.set_lists_suggestions(ModelRc::new(CommonRows {
        view: view.clone(),
        notify: ModelNotify::default(),
    }));
    window.set_lists_tasks(ModelRc::new(Tasks {
        view: view.clone(),
        notify: ModelNotify::default(),
    }));
    window.set_lists_task_selected(selected.is_some());
    window.set_lists_active_index(selected.map_or(-1, |index| index as i32));
    window.set_lists_task_selection(selection.clone());
    let draft = view
        .selected
        .as_ref()
        .and_then(|key| view.drafts.get(key))
        .map(|draft| draft.text.clone())
        .unwrap_or_else(|| selection.title.to_string());
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

    #[test]
    fn selected_catalogue_identity_is_the_visible_list_selector_label() {
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
        assert!(ElementQuery::from_root(&window).match_inherits("KitSelectTrigger").match_predicate(move |e|e.accessible_label().as_deref()==Some(expected.as_str())).find_first().is_some());
        state.clear();
        render(&window, Projection::new(&state));
        assert!(
            ElementQuery::from_root(&window)
                .match_inherits("KitSelectTrigger")
                .match_predicate(|e| e.accessible_label().as_deref() == Some("Choose a list"))
                .find_first()
                .is_some()
        );
        window.hide().unwrap();
    }

    #[test]
    fn picker_search_filters_real_rows_preserves_selection_and_resets_visible_query() {
        use i_slint_backend_testing::{ElementHandle, ElementQuery};
        use slint::platform::{Key, PointerEventButton, WindowEvent};
        use std::{cell::RefCell, rc::Rc};
        i_slint_backend_testing::init_no_event_loop();
        let window = crate::create_window().unwrap();
        window.on_lists_filter_options(crate::list_host::filter_options);
        window.on_lists_find_option(crate::list_host::find_option);
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_endpoint_ready(true);
        window.set_show_endpoint(false);
        window.set_page(crate::ViewPage::Lists);
        window.set_lists_catalog_ready(true);
        window.set_lists_generation(7);
        window.set_lists_list_token(22);
        window.set_lists_option_index(1);
        let rows = |generation| {
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
                    ..Default::default()
                },
                OptionRow {
                    token: 33,
                    generation,
                    label: "Gamma store".into(),
                    ..Default::default()
                },
            ])
        };
        window.set_lists_options(rows(7));
        let calls = Rc::new(RefCell::new(Vec::new()));
        let out = calls.clone();
        window.on_lists_choose(move |t, g| out.borrow_mut().push((t, g)));
        window.show().unwrap();
        let trigger = || {
            ElementQuery::from_root(&window)
                .match_inherits("KitSelectTrigger")
                .match_predicate(|e| e.accessible_label().as_deref() == Some("Beta store"))
                .find_first()
                .unwrap()
        };
        let search = || {
            ElementQuery::from_root(&window)
                .match_inherits("TextInput")
                .match_predicate(|e| e.accessible_label().as_deref() == Some("Search options"))
                .find_first()
                .unwrap()
        };
        let options = || {
            ElementQuery::from_root(&window)
                .match_inherits("KitOption")
                .find_all()
        };
        let key = |text: slint::SharedString| {
            window
                .window()
                .dispatch_event(WindowEvent::KeyPressed { text: text.clone() });
            window
                .window()
                .dispatch_event(WindowEvent::KeyReleased { text });
        };
        let open = || {
            trigger().mock_single_click(PointerEventButton::Left);
            slint::platform::update_timers_and_animations();
        };
        let type_query = |field: ElementHandle, text: &str| {
            field.mock_single_click(PointerEventButton::Left);
            key(text.into());
        };
        open();
        assert_eq!(options().len(), 3);
        type_query(search(), "ALP");
        assert_eq!(options().len(), 1);
        assert!(trigger().is_valid());
        key(Key::DownArrow.into());
        key(Key::Return.into());
        assert_eq!(*calls.borrow(), [(11, 7)]);
        assert_eq!(
            window.get_lists_list_token(),
            22,
            "view filtering cannot select a list itself"
        );
        open();
        assert_eq!(options().len(), 3);
        assert_eq!(search().accessible_value().as_deref(), Some(""));
        type_query(search(), "no match");
        assert!(options().is_empty());
        key(Key::Return.into());
        assert_eq!(calls.borrow().len(), 1);
        window.set_lists_generation(8);
        window.set_lists_options(rows(8));
        slint::platform::update_timers_and_animations();
        open();
        assert_eq!(options().len(), 3);
        assert_eq!(search().accessible_value().as_deref(), Some(""));
        key(Key::Escape.into());
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
    let rows = ModelRc::new(WidgetTasks {
        indices: visible_indices(&view, target.shopping_only, target.limit),
        view,
        notify: ModelNotify::default(),
    });
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
        let common = CommonRows {
            view,
            notify: ModelNotify::default(),
        };
        assert_eq!(common.row_data(0).unwrap().token, state.item_tokens[0]);
        assert!(common.row_data(0).unwrap().enabled);
        assert!(!common.row_data(1).unwrap().enabled);
        assert_eq!(
            common.row_data(1).unwrap().status.as_str(),
            "Already on this list"
        );
        assert_eq!(
            state.items.as_ref().unwrap().count,
            4,
            "filtering never rewrites server count"
        );
    }
}
