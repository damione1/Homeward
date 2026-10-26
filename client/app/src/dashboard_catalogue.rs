//! An authorized List/Get cohort. Opaque tokens are compared, never interpreted.
use crate::dashboard::{DashboardId, Layout, LayoutFailure, LayoutState};
use buffa::Message;
use home_wire_contract::home::dashboard::v1 as wire;
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
    layouts: BTreeMap<DashboardId, LayoutState>,
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
    pub fn state(&self, id: &DashboardId) -> LayoutState {
        self.layouts
            .get(id)
            .cloned()
            .unwrap_or(LayoutState::Unavailable)
    }
    pub fn contains(&self, layout: &Layout) -> bool {
        self.layouts.get(&layout.id).and_then(LayoutState::layout) == Some(layout)
    }
    pub fn layout(&self, id: &DashboardId) -> Option<Arc<Layout>> {
        self.layouts.get(id)?.shared_layout()
    }
}

pub(crate) enum CaptureOutcome {
    Accepted,
    Changed,
}
enum CaptureState {
    Collecting,
    Invalidated,
}
pub(crate) struct Capture {
    token: CatalogueToken,
    expected: BTreeMap<DashboardId, wire::Metadata>,
    layouts: BTreeMap<DashboardId, LayoutState>,
    bytes: u64,
    state: CaptureState,
}
impl Capture {
    pub fn new(list: &wire::ListDashboardsResponse) -> Result<Self, LayoutFailure> {
        // Reuse the catalogue boundary, including duplicate IDs and its limit.
        crate::dashboard_selection::Catalogue::from_wire(list, None)?;
        let token = CatalogueToken::parse(&list.resume_token)?;
        let mut expected = BTreeMap::new();
        for metadata in &list.dashboards {
            let id =
                DashboardId::parse(&metadata.id.as_option().ok_or(LayoutFailure::Invalid)?.value)?;
            expected.insert(id, metadata.clone());
        }
        Ok(Self {
            token,
            expected,
            layouts: BTreeMap::new(),
            bytes: u64::from(list.encoded_len()),
            state: CaptureState::Collecting,
        })
    }
    pub fn insert(
        &mut self,
        requested: &DashboardId,
        response: &wire::GetDashboardResponse,
    ) -> Result<CaptureOutcome, LayoutFailure> {
        match self.state {
            CaptureState::Invalidated => return Err(LayoutFailure::Invalid),
            CaptureState::Collecting => {}
        }
        let result = self.insert_current(requested, response);
        match &result {
            Ok(CaptureOutcome::Accepted) => {}
            Ok(CaptureOutcome::Changed)
            | Err(LayoutFailure::Invalid | LayoutFailure::Unsupported | LayoutFailure::Budget) => {
                self.state = CaptureState::Invalidated
            }
        }
        result
    }
    fn insert_current(
        &mut self,
        requested: &DashboardId,
        response: &wire::GetDashboardResponse,
    ) -> Result<CaptureOutcome, LayoutFailure> {
        if !response.__buffa_unknown_fields.is_empty() {
            return Err(LayoutFailure::Unsupported);
        }
        if CatalogueToken::parse(&response.resume_token)? != self.token {
            return Ok(CaptureOutcome::Changed);
        }
        self.bytes = self.bytes.saturating_add(u64::from(response.encoded_len()));
        if self.bytes > MAX_CAPTURE_BYTES {
            return Err(LayoutFailure::Budget);
        }
        let document = response
            .dashboard
            .as_option()
            .ok_or(LayoutFailure::Invalid)?;
        let metadata = document
            .metadata
            .as_option()
            .ok_or(LayoutFailure::Invalid)?;
        let id = DashboardId::parse(&metadata.id.as_option().ok_or(LayoutFailure::Invalid)?.value)?;
        if &id != requested
            || self.expected.get(&id) != Some(metadata)
            || self.layouts.contains_key(&id)
        {
            return Err(LayoutFailure::Invalid);
        }
        let state = match Layout::try_from(document) {
            Ok(layout) => {
                // Even a misbehaving server cannot reuse one token for two
                // incompatible active configurations or authorization views.
                if self
                    .layouts
                    .values()
                    .filter_map(LayoutState::layout)
                    .any(|other| {
                        (
                            other.config_revision,
                            other.authorization_revision,
                            other.config_hash,
                        ) != (
                            layout.config_revision,
                            layout.authorization_revision,
                            layout.config_hash,
                        )
                    })
                {
                    return Err(LayoutFailure::Invalid);
                }
                LayoutState::Ready(Arc::new(layout))
            }
            Err(LayoutFailure::Unsupported) => LayoutState::unsupported(document),
            Err(error @ (LayoutFailure::Invalid | LayoutFailure::Budget)) => return Err(error),
        };
        self.layouts.insert(id, state);
        Ok(CaptureOutcome::Accepted)
    }
    pub fn finish(self) -> Result<Arc<Snapshot>, LayoutFailure> {
        match self.state {
            CaptureState::Invalidated => return Err(LayoutFailure::Invalid),
            CaptureState::Collecting => {}
        }
        if self.expected.len() != self.layouts.len() {
            return Err(LayoutFailure::Invalid);
        }
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
