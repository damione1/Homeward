use crate::{
    ContentItem, HomewardWindow, ProTaskAccess, ProTaskPhase, ProTaskRow,
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
            notice: [state.read_notice, state.notice]
                .into_iter()
                .filter(|text| !text.is_empty())
                .collect::<Vec<_>>()
                .join(" "),
        }
    }
    fn rows(&self) -> &[Arc<home_wire_contract::home::lists::v1::ItemSnapshot>] {
        self.items
            .as_ref()
            .map_or(&[], |snapshot| match &snapshot.rows {
                CapturedRows::Items { rows, .. } => rows,
                CapturedRows::Catalog(_) => &[],
            })
    }
    fn row(&self, index: usize) -> Option<ProTaskRow> {
        let row = self.rows().get(index)?;
        let key = ItemKey::new(row.list_id.value.parse().ok()?, row.uid.value.parse().ok()?);
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
            title: row.value.summary.clone().into(),
            completed: row.value.completed == Some(true),
            writable: true,
            detail: row.value.description.clone().unwrap_or_default().into(),
            phase,
            status: match phase {
                ProTaskPhase::Ready => {
                    if draft.is_some() {
                        "Brouillon non enregistré"
                    } else {
                        ""
                    }
                }
                ProTaskPhase::Pending => "En attente du serveur",
                ProTaskPhase::Conflict => "Conflit à résoudre",
                ProTaskPhase::CommitUnknown => "Résultat inconnu · aucun renvoi",
                ProTaskPhase::CommittedResponseWithheld => "Accès retiré",
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
            title: row.name.clone().into(),
            link_label: "Ouvrir".into(),
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
    let old_focus = window.get_lists_editor_focused();
    let old_token = window.get_lists_task_selection().token;
    let old_scroll = window.get_lists_scroll_y();
    let selected = view.selected.as_ref().and_then(|key| {
        view.rows().iter().position(|row| {
            row.uid.value == key.uid().as_str() && row.list_id.value == key.list().as_str()
        })
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
            .map_or_else(String::new, |snapshot| format!("{} listes", snapshot.count))
            .into(),
    );
    window.set_lists_item_count(
        view.items
            .as_ref()
            .map_or_else(String::new, |snapshot| {
                format!("{} éléments", snapshot.count)
            })
            .into(),
    );
    window.set_lists_selected(view.items.is_some());
    window.set_lists_title(
        view.items
            .as_ref()
            .and_then(|snapshot| match &snapshot.metadata {
                ListReadMetadata::Catalog => None,
                ListReadMetadata::List(list) => Some(list.name.clone()),
            })
            .unwrap_or_else(|| "Listes".into())
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
    window.invoke_lists_scroll_to(old_scroll);
    if old_focus && selection.token == old_token && selected.is_some() {
        window.invoke_lists_focus_editor();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use slint::ComponentHandle;

    #[test]
    fn a_coalesced_active_projection_still_erases_all_header_drafts_after_a_private_reset() {
        i_slint_backend_testing::init_no_event_loop();
        let window = HomewardWindow::new().unwrap();
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
        let window = HomewardWindow::new().unwrap();
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
