//! One private camera projection. No pixels enter StateStore, Recorder or disk.
use crate::raster::{self, Pixels};
use home_client_model::{
    api_content::MediaSourceName,
    cameras::{EventKey, ImageKey},
};
use home_wire_contract::camera_content as admit;
use std::{collections::BTreeMap, sync::Arc, time::Instant};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Status {
    Hidden,
    Loading,
    Ready,
    Missing,
    Expired,
    Revoked,
    WrongCamera,
    Offline,
    Unavailable,
    Invalid,
    Unsupported,
}
#[derive(Clone)]
pub(crate) struct Frame {
    pub key: ImageKey,
    pub session: home_client_model::api_content::MediaSessionName,
    pub captured_at: std::time::SystemTime,
    pub deadline: Instant,
    pub pixels: Pixels,
}
pub(crate) fn decode(image: admit::Image, now: Instant) -> Result<Frame, Status> {
    if now >= image.deadline {
        return Err(Status::Expired);
    }
    let pixels = raster::decode_png(
        &image.png,
        raster::Limits {
            encoded: admit::MAX_IMAGE_BYTES,
            side: admit::MAX_IMAGE_SIDE,
            decoded: admit::MAX_IMAGE_SIDE as usize * admit::MAX_IMAGE_SIDE as usize * 4,
            decoder: 8 * 1024 * 1024,
        },
    )
    .map_err(|error| match error {
        raster::Failure::Invalid | raster::Failure::Capacity => Status::Invalid,
    })?;
    Ok(Frame {
        key: image.key,
        session: image.session,
        captured_at: image.captured_at,
        deadline: image.deadline,
        pixels,
    })
}

/// Stable event tokens map to keys; a visual row position is never identity.
#[derive(Clone, Default)]
pub(crate) struct Events {
    pub rows: Arc<Vec<admit::Event>>,
    tokens: Arc<Vec<i32>>,
    keys: Arc<BTreeMap<i32, EventKey>>,
    next: i32,
}
impl Events {
    pub fn new(rows: Vec<admit::Event>, previous: &Self) -> Result<Self, Status> {
        if rows.len() > admit::MAX_EVENTS {
            return Err(Status::Invalid);
        }
        let mut identities = BTreeMap::new();
        let mut keys = BTreeMap::new();
        let mut tokens = Vec::with_capacity(rows.len());
        let old: BTreeMap<_, _> = previous
            .keys
            .iter()
            .map(|(token, key)| (key.clone(), *token))
            .collect();
        let mut next = previous.next;
        for event in &rows {
            let token = match old.get(&event.key) {
                Some(token) => *token,
                None => {
                    next = next.checked_add(1).ok_or(Status::Invalid)?;
                    next
                }
            };
            if identities.insert(event.key.clone(), token).is_some() {
                return Err(Status::Invalid);
            }
            keys.insert(token, event.key.clone());
            tokens.push(token);
        }
        Ok(Self {
            rows: Arc::new(rows),
            tokens: Arc::new(tokens),
            keys: Arc::new(keys),
            next,
        })
    }
    pub fn token(&self, index: usize) -> Option<i32> {
        self.tokens.get(index).copied()
    }
    pub fn key(&self, token: i32) -> Option<&EventKey> {
        self.keys.get(&token)
    }
}

pub(crate) struct State {
    pub context: u64,
    pub generation: i32,
    pub foreground: bool,
    pub interactive: bool,
    pub preview_visible: bool,
    pub active: bool,
    pub catalog: Arc<Vec<admit::Camera>>,
    pub selected: Option<MediaSourceName>,
    pub target: Option<ImageKey>,
    pub events: Events,
    pub picture: Option<Arc<Frame>>,
    pub status: Status,
    pub catalog_status: Status,
    pub cancel: CancellationToken,
    pub image_cancel: CancellationToken,
    pub read_requested: bool,
    pub image_requested: bool,
}
impl Default for State {
    fn default() -> Self {
        let cancel = CancellationToken::new();
        Self {
            context: 1,
            generation: 1,
            foreground: true,
            interactive: false,
            preview_visible: true,
            active: false,
            catalog: Arc::default(),
            selected: None,
            target: None,
            events: Events::default(),
            picture: None,
            status: Status::Hidden,
            catalog_status: Status::Hidden,
            image_cancel: cancel.child_token(),
            cancel,
            read_requested: false,
            image_requested: false,
        }
    }
}
impl State {
    pub fn replace_events(&mut self, rows: Vec<admit::Event>) -> Result<(), Status> {
        self.events = Events::new(rows, &self.events)?;
        if let Some(ImageKey::EventThumbnail(key)) = &self.target {
            let unavailable = match self.events.rows.iter().find(|event| &event.key == key) {
                None => Some(Status::Missing),
                Some(event) if !event.has_image => Some(Status::Unavailable),
                Some(_) => None,
            };
            if let Some(status) = unavailable {
                self.image_cancel.cancel();
                self.image_cancel = CancellationToken::new();
                self.picture = None;
                self.status = status;
                self.image_requested = false;
            }
        }
        Ok(())
    }
    pub fn bump(&mut self) {
        if self.generation == 0 {
            return;
        }
        self.generation = self.generation.checked_add(1).unwrap_or(0);
        if self.generation == 0 {
            self.clear_private();
        }
    }
    /// Retire publication before joining any outstanding HTTP/decode jobs.
    pub fn clear_private(&mut self) {
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.image_cancel.cancel();
        self.image_cancel = self.cancel.child_token();
        self.context = if self.context == 0 {
            0
        } else {
            self.context.checked_add(1).unwrap_or(0)
        };
        self.generation = if self.generation == 0 {
            0
        } else {
            self.generation.checked_add(1).unwrap_or(0)
        };
        self.catalog = Arc::default();
        self.selected = None;
        self.target = None;
        self.events = Events::default();
        self.picture = None;
        self.active = false;
        self.status = Status::Hidden;
        self.catalog_status = Status::Hidden;
        self.read_requested = false;
        self.image_requested = false;
    }
    pub fn select(&mut self, id: MediaSourceName) {
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.image_cancel.cancel();
        self.image_cancel = self.cancel.child_token();
        self.context = self.context.checked_add(1).unwrap_or(0);
        self.bump();
        self.selected = Some(id.clone());
        self.target = Some(ImageKey::Snapshot(id));
        self.events = Events::default();
        self.picture = None;
        self.status = Status::Loading;
        self.read_requested = true;
        self.image_requested = true;
    }
    pub fn set_target(&mut self, key: ImageKey) {
        self.image_cancel.cancel();
        self.image_cancel = self.cancel.child_token();
        self.picture = None;
        self.target = Some(key);
        self.status = Status::Loading;
        self.image_requested = true;
        self.bump();
    }
    pub fn hide_preview(&mut self) {
        self.image_cancel.cancel();
        self.picture = None;
        self.image_requested = false;
        self.status = match self.status {
            Status::Ready | Status::Loading | Status::Hidden => Status::Hidden,
            Status::Missing => Status::Missing,
            Status::Expired => Status::Expired,
            Status::Revoked => Status::Revoked,
            Status::WrongCamera => Status::WrongCamera,
            Status::Offline => Status::Offline,
            Status::Unavailable => Status::Unavailable,
            Status::Invalid => Status::Invalid,
            Status::Unsupported => Status::Unsupported,
        };
        self.bump();
    }
    pub fn accepts(&self, context: u64, now: Instant) -> bool {
        self.context == context
            && self.context != 0
            && self.generation > 0
            && self.active
            && self.foreground
            && self.interactive
            && !self.cancel.is_cancelled()
            && self.picture.as_ref().is_none_or(|p| p.deadline > now)
    }
}
