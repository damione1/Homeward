use super::*;
use home_client_actions::lists::{NotSentReason, WriteIssue};
use home_client_transport::FailureKind;
use std::time::SystemTime;

pub(crate) const LIST: &str = " list / α ";
fn list(id: &str, revision: u64) -> wire::ListSnapshot {
    let mut list = wire::ListSnapshot {
        name: format!("Synthetic {id}"),
        origin: wire::ListOrigin::Native.into(),
        ..Default::default()
    };
    list.id.get_or_insert_default().value = id.into();
    list.revision.get_or_insert_default().list_id = list.id.clone();
    list.revision.get_or_insert_default().number = revision;
    list.updated_at.get_or_insert_default().seconds = 1;
    list
}
pub(crate) fn catalog(ids: &[&str]) -> Arc<ListReadSnapshot> {
    Arc::new(ListReadSnapshot {
        metadata: ListReadMetadata::Catalog,
        rows: CapturedRows::Catalog(ids.iter().map(|id| Arc::new(list(id, 10))).collect()),
        count: ids.len() as u64,
        fetched_at: SystemTime::UNIX_EPOCH,
    })
}
pub(crate) fn items(
    id: &str,
    revision: u64,
    values: &[(&str, u64, &str)],
) -> Arc<ListReadSnapshot> {
    let rows = values
        .iter()
        .enumerate()
        .map(|(index, (uid, revision, summary))| {
            let mut item = wire::ItemSnapshot {
                position: index as i64,
                ..Default::default()
            };
            item.list_id.get_or_insert_default().value = id.into();
            item.uid.get_or_insert_default().value = (*uid).into();
            item.revision.get_or_insert_default().list_id = item.list_id.clone();
            item.revision.get_or_insert_default().item_uid = item.uid.clone();
            item.revision.get_or_insert_default().number = *revision;
            item.value.get_or_insert_default().summary = (*summary).into();
            item.value.get_or_insert_default().completed = Some(false);
            item.value.get_or_insert_default().description = Some(String::new());
            Arc::new(item)
        })
        .collect();
    Arc::new(ListReadSnapshot {
        metadata: ListReadMetadata::List(Arc::new(list(id, revision))),
        rows: CapturedRows::Items {
            list: id.parse().unwrap(),
            rows,
        },
        count: values.len() as u64,
        fetched_at: SystemTime::UNIX_EPOCH,
    })
}
pub(crate) fn ready() -> State {
    let mut state = State {
        active: true,
        ..Default::default()
    };
    state
        .publish_catalog(catalog(&[LIST, "other-list"]))
        .unwrap();
    state
        .select_list(state.catalog_tokens[0], state.generation)
        .unwrap();
    state
        .publish_items(items(
            LIST,
            10,
            &[("same", 3, "Server one"), ("another", 2, "Server two")],
        ))
        .unwrap();
    state
        .select_item(state.item_tokens[0], state.generation)
        .unwrap();
    state
}
fn dirty() -> State {
    let mut state = ready();
    state
        .edit(state.item_tokens[0], state.generation, "Local draft".into())
        .unwrap();
    state
}

#[test]
fn tokens_and_revisions_cannot_retarget_an_equal_uid_in_another_list() {
    let mut state = dirty();
    let token = state.item_tokens[0];
    let generation = state.generation;
    let prepared = state.prepare(generation, Write::Save).unwrap();
    assert!(state.admits(prepared.context, prepared.id));
    state.finish(
        prepared.context,
        prepared.id,
        ListWriteOutcome::NotSent(NotSentReason::AdmissionChanged),
    );
    state
        .select_list(state.catalog_tokens[1], state.generation)
        .unwrap();
    state
        .publish_items(items("other-list", 10, &[("same", 3, "Different task")]))
        .unwrap();
    assert_ne!(state.item_tokens[0], token);
    assert_eq!(
        state.select_item(token, state.generation),
        Err(Refusal::Stale)
    );
    assert_eq!(
        state.select_item(state.item_tokens[0], generation),
        Err(Refusal::Stale)
    );
    assert!(!state.admits(prepared.context, prepared.id));
    state
        .select_item(state.item_tokens[0], state.generation)
        .unwrap();
    assert_eq!(
        state.selected_target().unwrap().1.key().list().as_str(),
        "other-list"
    );
}

#[test]
fn conflict_keeps_the_draft_and_requires_rereading_then_explicit_resolution() {
    let mut state = dirty();
    let prepared = state.prepare(state.generation, Write::Save).unwrap();
    state.finish(prepared.context, prepared.id, ListWriteOutcome::Conflict);
    state
        .publish_items(items(
            LIST,
            11,
            &[
                ("same", 4, "Concurrent server edit"),
                ("another", 2, "Server two"),
            ],
        ))
        .unwrap();
    let key = state.selected_item.clone().unwrap();
    let draft = state.drafts.get(&key).unwrap();
    assert_eq!(draft.text, "Local draft");
    assert!(draft.phase == DraftPhase::Conflict);
    assert_eq!(draft.base.item_revision().number().get(), 3);
    assert!(matches!(
        state.prepare(state.generation, Write::Save),
        Err(Refusal::Stale)
    ));
    state.resolve(state.generation).unwrap();
    let draft = state.drafts.get(&key).unwrap();
    assert_eq!(draft.text, "Local draft");
    assert_eq!(draft.base.item_revision().number().get(), 4);
    assert_eq!(draft.base.list_revision().number().get(), 11);
    assert!(!state.busy(), "resolution itself must not submit a write");
    assert!(state.prepare(state.generation, Write::Save).is_ok());
}

#[test]
fn snapshot_changes_preserve_dirty_text_and_never_silently_rebase_its_preconditions() {
    let mut state = dirty();
    let key = state.selected_item.clone().unwrap();
    state
        .publish_items(items(
            LIST,
            20,
            &[("same", 9, "Another actor"), ("another", 2, "Server two")],
        ))
        .unwrap();
    let draft = state.drafts.get(&key).unwrap();
    assert_eq!(draft.text, "Local draft");
    assert_eq!(draft.base.list_revision().number().get(), 10);
    assert_eq!(draft.base.item_revision().number().get(), 3);
    assert!(draft.phase == DraftPhase::Conflict);
    state
        .edit(
            state.item_tokens[0],
            state.generation,
            "Reworked draft".into(),
        )
        .unwrap();
    assert!(state.drafts[&key].phase == DraftPhase::Conflict);
}

#[test]
fn offline_withdraws_a_prepared_write_and_unknown_outcomes_stay_unknown_after_reread() {
    let mut state = dirty();
    let prepared = state.prepare(state.generation, Write::Save).unwrap();
    state.item_phase = Freshness::Syncing;
    assert!(
        state.admits(prepared.context, prepared.id),
        "an own Watch notification does not erase a submission opportunity"
    );
    state.item_phase = Freshness::Offline;
    assert!(!state.admits(prepared.context, prepared.id));
    state.finish(
        prepared.context,
        prepared.id,
        ListWriteOutcome::CommitUnknown(WriteIssue::Transport(FailureKind::NetworkInterrupted)),
    );
    assert!(matches!(
        state.prepare(state.generation, Write::Save),
        Err(Refusal::Stale)
    ));
    state.read_notice = "Offline read";
    state
        .publish_items(items(
            LIST,
            11,
            &[("same", 4, "Local draft"), ("another", 2, "Server two")],
        ))
        .unwrap();
    let key = state.selected_item.clone().unwrap();
    assert!(state.drafts[&key].phase == DraftPhase::Unknown);
    assert!(state.notice.contains("inconnu"));
    assert!(state.read_notice.is_empty());
    assert!(matches!(
        state.prepare(state.generation, Write::Save),
        Err(Refusal::Stale)
    ));
    state.reload(state.generation).unwrap();
    state
        .publish_items(items(
            LIST,
            11,
            &[("same", 4, "Local draft"), ("another", 2, "Server two")],
        ))
        .unwrap();
    assert!(!state.drafts.contains_key(&key));
    assert!(!state.busy());
    state.catalog_phase = Freshness::Offline;
    assert!(matches!(
        state.prepare(state.generation, Write::Complete(false)),
        Err(Refusal::Stale)
    ));
}

#[test]
fn authority_loss_removes_every_projection_and_invalidates_late_results() {
    let mut state = dirty();
    let prepared = state.prepare(state.generation, Write::Save).unwrap();
    let privacy = state.privacy_generation;
    state.finish(
        prepared.context,
        prepared.id,
        ListWriteOutcome::CommittedResponseWithheld,
    );
    assert!(!state.active);
    assert!(state.items.is_none() && state.catalog.is_none());
    assert!(state.drafts.is_empty() && state.tokens.is_empty() && state.list_tokens.is_empty());
    assert!(state.selected_item.is_none() && state.selected_list.is_none());
    assert!(state.privacy_generation > privacy);
    state.active = true;
    state.publish_catalog(catalog(&[LIST])).unwrap();
    assert!(!state.admits(prepared.context, prepared.id));
    state.finish(prepared.context, prepared.id, ListWriteOutcome::Conflict);
    assert!(state.drafts.is_empty());
}

#[test]
fn deleting_or_forbidding_a_list_removes_its_drafts_and_pending_admission() {
    let mut state = dirty();
    let prepared = state.prepare(state.generation, Write::Save).unwrap();
    state.publish_catalog(catalog(&["other-list"])).unwrap();
    assert!(
        state.items.is_none() && state.selected_list.is_none() && state.selected_item.is_none()
    );
    assert!(state.drafts.is_empty());
    assert!(!state.admits(prepared.context, prepared.id));
    assert!(state.item_tokens.is_empty());
}

#[test]
fn movement_keeps_stable_identity_and_removal_cannot_transfer_the_draft_to_its_successor() {
    let mut state = dirty();
    let token = state.item_tokens[0];
    let key = state.selected_item.clone().unwrap();
    state
        .publish_items(items(
            LIST,
            11,
            &[("another", 4, "Server two"), ("same", 4, "Server one")],
        ))
        .unwrap();
    assert_eq!(state.item_tokens[1], token);
    assert_eq!(state.selected_item, Some(key.clone()));
    assert_eq!(state.drafts[&key].text, "Local draft");
    state
        .publish_items(items(LIST, 12, &[("another", 4, "Server two")]))
        .unwrap();
    assert!(state.selected_item.is_none());
    assert!(!state.drafts.contains_key(&key));
    assert_eq!(
        state.select_item(token, state.generation),
        Err(Refusal::Stale)
    );
}

#[test]
fn one_in_flight_write_blocks_new_opportunities_and_foreign_results_cannot_release_it() {
    let mut state = dirty();
    let prepared = state.prepare(state.generation, Write::Save).unwrap();
    assert!(matches!(
        state.prepare(state.generation, Write::Complete(true)),
        Err(Refusal::Busy)
    ));
    state.finish(
        prepared.context,
        prepared.id + 1,
        ListWriteOutcome::Conflict,
    );
    assert!(state.busy());
    state.finish(
        prepared.context,
        prepared.id,
        ListWriteOutcome::NotSent(NotSentReason::Cancelled),
    );
    assert!(!state.busy());
    assert_eq!(state.next_write, 1);
}

#[test]
fn exhausted_contexts_and_generations_stay_closed_after_repeated_resets() {
    let mut state = ready();
    state.context = u64::MAX;
    state.generation = i32::MAX;
    state.clear();
    state.clear();
    assert_eq!(state.context, 0);
    assert_eq!(state.generation, 0);
    state.active = true;
    state.catalog_phase = Freshness::Online;
    assert!(matches!(
        state.prepare(0, Write::Create("Never sent".into())),
        Err(Refusal::Stale)
    ));
}

#[test]
fn a_large_model_and_many_selections_keep_dirty_drafts_bounded_without_eviction() {
    let mut state = ready();
    let names = (0..10_000).map(|i| format!("task-{i}")).collect::<Vec<_>>();
    let values = names
        .iter()
        .map(|name| (name.as_str(), 1, "Synthetic task"))
        .collect::<Vec<_>>();
    state.publish_items(items(LIST, 10, &values)).unwrap();
    for index in 0..128 {
        let token = state.item_tokens[index];
        state.select_item(token, state.generation).unwrap();
        state
            .edit(token, state.generation, format!("Draft {index}"))
            .unwrap();
    }
    let token = state.item_tokens[128];
    state.select_item(token, state.generation).unwrap();
    assert_eq!(
        state.edit(token, state.generation, "Capacity".into()),
        Err(Refusal::Capacity)
    );
    assert_eq!(state.drafts.len(), 128);
    assert_eq!(state.tokens.len(), 10_000);
    state.publish_catalog(catalog(&[])).unwrap();
    assert!(state.drafts.is_empty() && state.tokens.is_empty());
}

#[test]
fn resync_resumes_only_an_identity_read_again_in_a_fresh_authorized_catalogue() {
    let mut state = dirty();
    let key = state.selected_item.clone().unwrap();
    state.reset_for_resync();
    assert!(state.catalog.is_none() && state.items.is_none() && state.drafts.is_empty());
    assert!(state.selected_list.is_none());
    state.active = true;
    state.publish_catalog(catalog(&[LIST])).unwrap();
    assert_eq!(state.selected_list.as_ref().unwrap().as_str(), LIST);
    assert!(
        state.items.is_none(),
        "a fresh item capture is required; the old cache is not restored"
    );
    state
        .publish_items(items(LIST, 12, &[("same", 4, "Fresh authorized value")]))
        .unwrap();
    assert!(!state.drafts.contains_key(&key));
    assert!(state.notice.contains("brouillons"));
    state.reset_for_resync();
    state.active = true;
    state.publish_catalog(catalog(&["other-list"])).unwrap();
    assert!(state.selected_list.is_none() && state.items.is_none());
}

#[test]
fn resync_during_a_write_reports_uncertainty_and_never_revives_its_admission() {
    let mut state = dirty();
    let prepared = state.prepare(state.generation, Write::Save).unwrap();
    state.reset_for_resync();
    assert!(state.notice.contains("résultat inconnu"));
    state.active = true;
    state.publish_catalog(catalog(&[LIST])).unwrap();
    assert!(!state.admits(prepared.context, prepared.id));
    assert!(!state.busy());
}
