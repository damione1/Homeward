//! An authorized List/Get cohort. Opaque tokens are compared, never interpreted.
use crate::dashboard::{DashboardName, Layout, LayoutFailure, LayoutState};
use buffa::Message;
use home_wire_contract::home::api::v2 as wire;
use std::{collections::BTreeMap, sync::Arc};

const MAX_CAPTURE_BYTES: u64 = 16 * 1024 * 1024;

#[derive(Clone, PartialEq, Eq)]
pub(crate) struct CatalogueToken(Arc<str>);
impl std::fmt::Debug for CatalogueToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("CatalogueToken([redacted])")
    }
}
impl CatalogueToken {
    fn parse(value: &str) -> Result<Self, LayoutFailure> {
        if value.is_empty() || value.len() > 4096 || value.chars().any(char::is_control) {
            return Err(LayoutFailure::Invalid);
        }
        Ok(Self(value.into()))
    }
}

#[derive(Clone, PartialEq, Eq)]
pub(crate) struct Snapshot {
    token: CatalogueToken,
    layouts: BTreeMap<DashboardName, LayoutState>,
}
impl std::fmt::Debug for Snapshot {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DashboardSnapshot")
            .field("token", &self.token)
            .field("dashboards", &self.layouts.len())
            .finish()
    }
}
impl Snapshot {
    /// A renewed opaque checkpoint is not a change to the authorized catalogue.
    /// Preserve routes only when every admitted document is still identical.
    pub fn same_documents(&self, other: &Self) -> bool {
        self.layouts.len() == other.layouts.len()
            && self.layouts.iter().all(|(name, state)| {
                let Some(next) = other.layouts.get(name) else {
                    return false;
                };
                if let (Some(before), Some(after)) = (state.layout(), next.layout()) {
                    before.same_document(after)
                } else {
                    state == next
                }
            })
    }

    #[cfg(test)]
    pub fn fixture(token: &str, layouts: Vec<Arc<Layout>>) -> Arc<Self> {
        let layouts = layouts
            .into_iter()
            .map(|layout| (layout.id.clone(), LayoutState::Ready(layout)))
            .collect();
        Arc::new(Self {
            token: CatalogueToken::parse(token).unwrap(),
            layouts,
        })
    }
    pub fn token(&self) -> &CatalogueToken {
        &self.token
    }
    pub fn state(&self, id: &DashboardName) -> LayoutState {
        self.layouts
            .get(id)
            .cloned()
            .unwrap_or(LayoutState::Unavailable)
    }
    pub fn contains(&self, layout: &Layout) -> bool {
        self.layouts.get(&layout.id).and_then(LayoutState::layout) == Some(layout)
    }
    pub fn layout(&self, id: &DashboardName) -> Option<Arc<Layout>> {
        self.layouts.get(id)?.shared_layout()
    }
}

pub(crate) enum CaptureOutcome {
    Accepted,
}
pub(crate) struct Capture {
    token: CatalogueToken,
    expected: BTreeMap<DashboardName, ()>,
    layouts: BTreeMap<DashboardName, LayoutState>,
    bytes: u64,
    view: Arc<home_client_cache::CachedView>,
    invalid: bool,
}
impl Capture {
    pub fn new(
        list: &wire::ListDashboardsResponse,
        view: Arc<home_client_cache::CachedView>,
    ) -> Result<Self, LayoutFailure> {
        crate::dashboard_selection::Catalogue::from_wire(list, None)?;
        if !list.next_page_token.is_empty() {
            return Err(LayoutFailure::Invalid);
        };
        let mut expected = BTreeMap::new();
        for document in &list.dashboards {
            let id = DashboardName::try_from(document.name.as_str())
                .map_err(|_| LayoutFailure::Invalid)?;
            if id.home() != view.name().home() {
                return Err(LayoutFailure::Invalid);
            };
            expected.insert(id, ());
        }
        let bytes = u64::from(list.encoded_len());
        if bytes > MAX_CAPTURE_BYTES {
            return Err(LayoutFailure::Budget);
        };
        Ok(Self {
            token: CatalogueToken::parse(&list.resume_token)?,
            expected,
            layouts: BTreeMap::new(),
            bytes,
            view,
            invalid: false,
        })
    }
    pub fn insert(
        &mut self,
        requested: &DashboardName,
        response: &wire::QueryDashboardResponse,
    ) -> Result<CaptureOutcome, LayoutFailure> {
        if self.invalid {
            return Err(LayoutFailure::Invalid);
        };
        let result = self.insert_current(requested, response);
        if result.is_err() {
            self.invalid = true
        };
        result
    }
    fn insert_current(
        &mut self,
        requested: &DashboardName,
        response: &wire::QueryDashboardResponse,
    ) -> Result<CaptureOutcome, LayoutFailure> {
        self.bytes = self.bytes.saturating_add(u64::from(response.encoded_len()));
        if self.bytes > MAX_CAPTURE_BYTES {
            return Err(LayoutFailure::Budget);
        };
        if !self.expected.contains_key(requested) || self.layouts.contains_key(requested) {
            return Err(LayoutFailure::Invalid);
        };
        let layout = Layout::join(response, requested, &self.view)?;
        self.layouts
            .insert(requested.clone(), LayoutState::Ready(Arc::new(layout)));
        Ok(CaptureOutcome::Accepted)
    }
    pub fn finish(self) -> Result<Arc<Snapshot>, LayoutFailure> {
        if self.invalid || self.expected.len() != self.layouts.len() {
            return Err(LayoutFailure::Invalid);
        };
        Ok(Arc::new(Snapshot {
            token: self.token,
            layouts: self.layouts,
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn name(id: &str) -> DashboardName {
        DashboardName::try_from(format!("homes/synthetic/dashboards/{id}").as_str()).unwrap()
    }
    fn reply(id: &str, view: &home_client_cache::CachedView) -> wire::QueryDashboardResponse {
        let mut document = crate::dashboard::fixture_wire();
        document.name = name(id).as_str().into();
        crate::dashboard::query_fixture(document, view)
    }
    fn list(replies: &[wire::QueryDashboardResponse]) -> wire::ListDashboardsResponse {
        wire::ListDashboardsResponse {
            dashboards: replies
                .iter()
                .map(|r| r.dashboard.as_option().unwrap().clone())
                .collect(),
            resume_token: "opaque-cohort-a".into(),
            ..Default::default()
        }
    }
    fn insert(
        capture: &mut Capture,
        response: &wire::QueryDashboardResponse,
    ) -> Result<CaptureOutcome, LayoutFailure> {
        capture.insert(
            &DashboardName::try_from(response.dashboard.name.as_str()).unwrap(),
            response,
        )
    }
    #[test]
    fn only_a_complete_list_query_capture_can_be_published_and_tokens_are_redacted() {
        let view = Arc::new(crate::dashboard::capture_fixture(vec![]));
        let replies = [reply("first", &view), reply("second", &view)];
        let mut capture = Capture::new(&list(&replies), view.clone()).unwrap();
        assert!(matches!(
            insert(&mut capture, &replies[1]),
            Ok(CaptureOutcome::Accepted)
        ));
        assert!(capture.finish().is_err());
        let mut capture = Capture::new(&list(&replies), view).unwrap();
        for reply in &replies {
            assert!(matches!(
                insert(&mut capture, reply),
                Ok(CaptureOutcome::Accepted)
            ));
        }
        let snapshot = capture.finish().unwrap();
        assert!(snapshot.layout(&name("first")).is_some());
        assert!(snapshot.layout(&name("absent")).is_none());
        assert!(!format!("{snapshot:?}").contains("opaque-cohort-a"));
    }
    #[test]
    fn changed_checkpoint_foreign_capture_missing_binding_and_expiry_poison_the_capture() {
        let view = Arc::new(crate::dashboard::capture_fixture(vec![]));
        let replies = [reply("first", &view), reply("second", &view)];
        for case in 0..4 {
            let mut capture = Capture::new(&list(&replies), view.clone()).unwrap();
            assert!(insert(&mut capture, &replies[0]).is_ok());
            let mut changed = replies[1].clone();
            match case {
                0 => changed.resume_token = "other-checkpoint".into(),
                1 => changed.entity_view = "homes/synthetic/entityViews/other-capture".into(),
                2 => changed.binding.clear(),
                _ => changed.expire_time.get_or_insert_default().seconds += 1,
            }
            assert!(insert(&mut capture, &changed).is_err());
            assert!(insert(&mut capture, &replies[1]).is_err());
            assert!(capture.finish().is_err());
        }
    }
    #[test]
    fn query_cannot_substitute_another_authorized_unlisted_or_duplicate_dashboard() {
        let view = Arc::new(crate::dashboard::capture_fixture(vec![]));
        let replies = [reply("first", &view), reply("second", &view)];
        let mut capture = Capture::new(&list(&replies), view.clone()).unwrap();
        assert!(capture.insert(&name("first"), &replies[1]).is_err());
        assert!(insert(&mut capture, &replies[0]).is_err());
        assert!(capture.finish().is_err());
        let mut capture = Capture::new(&list(&replies), view.clone()).unwrap();
        assert!(insert(&mut capture, &reply("unlisted", &view)).is_err());
        let mut capture = Capture::new(&list(&replies), view).unwrap();
        assert!(insert(&mut capture, &replies[0]).is_ok());
        assert!(insert(&mut capture, &replies[0]).is_err());
        assert!(capture.finish().is_err());
    }
    #[test]
    fn unknown_documents_refuse_publication_and_all_received_bytes_count_toward_budget() {
        let view = Arc::new(crate::dashboard::capture_fixture(vec![]));
        let replies = [reply("first", &view)];
        let mut bytes = replies[0].dashboard.encode_to_vec();
        bytes.extend_from_slice(&[0x98, 0x06, 0x01]);
        let mut unknown = replies[0].clone();
        unknown.dashboard = wire::Dashboard::decode_from_slice(&bytes).unwrap().into();
        let mut capture = Capture::new(&list(&replies), view.clone()).unwrap();
        assert!(insert(&mut capture, &unknown).is_err());
        assert!(capture.finish().is_err());
        let mut large = replies[0].clone();
        large.dashboard.get_or_insert_default().display_name =
            "x".repeat(MAX_CAPTURE_BYTES as usize);
        let mut capture = Capture::new(&list(&replies), view).unwrap();
        assert!(matches!(
            insert(&mut capture, &large),
            Err(LayoutFailure::Budget)
        ));
    }
}
