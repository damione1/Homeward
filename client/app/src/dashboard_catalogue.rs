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
    Changed,
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
    fn reply(id: &str) -> wire::GetDashboardResponse {
        let mut document = crate::dashboard::fixture_wire();
        document
            .metadata
            .as_option_mut()
            .unwrap()
            .id
            .as_option_mut()
            .unwrap()
            .value = id.into();
        wire::GetDashboardResponse {
            dashboard: document.into(),
            resume_token: "opaque-cohort-a".into(),
            ..Default::default()
        }
    }
    fn insert(
        capture: &mut Capture,
        response: &wire::GetDashboardResponse,
    ) -> Result<CaptureOutcome, LayoutFailure> {
        let id = DashboardId::parse(
            &response
                .dashboard
                .as_option()
                .unwrap()
                .metadata
                .as_option()
                .unwrap()
                .id
                .as_option()
                .unwrap()
                .value,
        )
        .unwrap();
        capture.insert(&id, response)
    }
    fn list(replies: &[wire::GetDashboardResponse]) -> wire::ListDashboardsResponse {
        wire::ListDashboardsResponse {
            dashboards: replies
                .iter()
                .map(|reply| {
                    reply
                        .dashboard
                        .as_option()
                        .unwrap()
                        .metadata
                        .as_option()
                        .unwrap()
                        .clone()
                })
                .collect(),
            resume_token: "opaque-cohort-a".into(),
            ..Default::default()
        }
    }
    #[test]
    fn only_a_complete_list_get_cohort_can_be_published_and_tokens_are_redacted() {
        let replies = [reply("first"), reply("second")];
        let mut capture = Capture::new(&list(&replies)).unwrap();
        assert!(matches!(
            insert(&mut capture, &replies[1]),
            Ok(CaptureOutcome::Accepted)
        ));
        assert!(capture.finish().is_err());
        let mut capture = Capture::new(&list(&replies)).unwrap();
        for reply in &replies {
            assert!(matches!(
                insert(&mut capture, reply),
                Ok(CaptureOutcome::Accepted)
            ));
        }
        let snapshot = capture.finish().unwrap();
        assert!(
            snapshot
                .layout(&DashboardId::parse("first").unwrap())
                .is_some()
        );
        assert!(
            snapshot
                .layout(&DashboardId::parse("absent").unwrap())
                .is_none()
        );
        assert!(!format!("{snapshot:?}").contains("opaque-cohort-a"));
    }
    #[test]
    fn a_changed_token_requires_a_new_capture_instead_of_mixing_documents() {
        let replies = [reply("first"), reply("second")];
        let mut capture = Capture::new(&list(&replies)).unwrap();
        assert!(matches!(
            insert(&mut capture, &replies[0]),
            Ok(CaptureOutcome::Accepted)
        ));
        let mut changed = replies[1].clone();
        changed.resume_token = "opaque-cohort-b".into();
        assert!(matches!(
            insert(&mut capture, &changed),
            Ok(CaptureOutcome::Changed)
        ));
        assert!(
            insert(&mut capture, &replies[1]).is_err(),
            "a changed capture cannot be repaired with a later matching response"
        );
        assert!(capture.finish().is_err());
    }
    #[test]
    fn get_cannot_return_a_different_even_if_authorized_dashboard_from_the_same_list() {
        let replies = [reply("first"), reply("second")];
        let mut capture = Capture::new(&list(&replies)).unwrap();
        assert!(matches!(
            capture.insert(&DashboardId::parse("first").unwrap(), &replies[1]),
            Err(LayoutFailure::Invalid)
        ));
        assert!(
            insert(&mut capture, &replies[0]).is_err(),
            "a contract violation poisons the entire capture"
        );
        assert!(capture.finish().is_err());
    }
    #[derive(Clone, Copy, Debug)]
    enum Contradiction {
        Metadata,
        Configuration,
        Authorization,
        Hash,
        Unlisted,
        Duplicate,
    }
    #[test]
    fn a_reused_token_cannot_admit_metadata_or_active_authority_contradictions() {
        for case in [
            Contradiction::Metadata,
            Contradiction::Configuration,
            Contradiction::Authorization,
            Contradiction::Hash,
            Contradiction::Unlisted,
            Contradiction::Duplicate,
        ] {
            let replies = [reply("first"), reply("second")];
            let mut capture = Capture::new(&list(&replies)).unwrap();
            assert!(matches!(
                insert(&mut capture, &replies[0]),
                Ok(CaptureOutcome::Accepted)
            ));
            let mut changed = replies[1].clone();
            let document = changed.dashboard.as_option_mut().unwrap();
            match case {
                Contradiction::Metadata => {
                    document
                        .metadata
                        .as_option_mut()
                        .unwrap()
                        .version
                        .as_option_mut()
                        .unwrap()
                        .revision += 1
                }
                Contradiction::Configuration => {
                    document
                        .native_overview
                        .as_option_mut()
                        .unwrap()
                        .config_revision += 1
                }
                Contradiction::Authorization => {
                    document
                        .native_overview
                        .as_option_mut()
                        .unwrap()
                        .authorization_revision += 1
                }
                Contradiction::Hash => {
                    document
                        .native_overview
                        .as_option_mut()
                        .unwrap()
                        .config_sha256 = vec![9; 32]
                }
                Contradiction::Unlisted => {
                    document
                        .metadata
                        .as_option_mut()
                        .unwrap()
                        .id
                        .as_option_mut()
                        .unwrap()
                        .value = "unlisted".into()
                }
                Contradiction::Duplicate => changed = replies[0].clone(),
            }
            assert!(
                matches!(insert(&mut capture, &changed), Err(LayoutFailure::Invalid)),
                "{case:?}"
            );
        }
    }
    #[test]
    fn unsupported_documents_stay_inert_and_preserved_source_counts_toward_the_capture_budget() {
        let mut replies = [reply("first"), reply("second")];
        replies[1]
            .dashboard
            .as_option_mut()
            .unwrap()
            .metadata
            .as_option_mut()
            .unwrap()
            .profile = "future-profile".into();
        let mut capture = Capture::new(&list(&replies)).unwrap();
        for reply in &replies {
            assert!(matches!(
                insert(&mut capture, reply),
                Ok(CaptureOutcome::Accepted)
            ));
        }
        let snapshot = capture.finish().unwrap();
        assert!(matches!(
            snapshot.state(&DashboardId::parse("second").unwrap()),
            LayoutState::Unsupported(_)
        ));
        let mut large = reply("large");
        large.dashboard.as_option_mut().unwrap().definition = wire::Definition {
            files: vec![wire::SourceFile {
                path: "synthetic.yaml".into(),
                exact_source: vec![b' '; MAX_CAPTURE_BYTES as usize],
                ..Default::default()
            }],
            ..Default::default()
        }
        .into();
        let mut capture = Capture::new(&list(&[large.clone()])).unwrap();
        assert!(matches!(
            insert(&mut capture, &large),
            Err(LayoutFailure::Budget)
        ));
    }
}
