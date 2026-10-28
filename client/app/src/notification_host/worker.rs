use super::*;
use buffa::EnumValue;

pub(super) enum Error {
    Transport(Failure),
    Cancelled,
    Invalid,
    Parent,
    Native(os::Error),
}
struct Run<'a> {
    owner: &'a Notifications,
    ticket: Ticket,
    session: &'a SessionCoordinator,
    connection: &'a Connection,
    lease: &'a AccessLease,
    stop: &'a CancellationToken,
    parent: &'a crate::companion_host::NotificationParent,
}
macro_rules! call {
    ($run:ident,$method:ident,$request:expr,$input:ident,$output:ident)=>{{
        let request=$request;
        admit::admit_request(admit::Request::$input(&request)).map_err(|_|Error::Invalid)?;
        $run.check()?;
        let api=$run.connection.notifications();
        let response=tokio::select!{biased;
            ()=$run.lease.cancellation.cancelled()=>return Err(Error::Cancelled),
            response=api.$method(request,$run.lease.token(),Duration::from_secs(5),$run.stop)=>response.map_err(Error::Transport)?,
        };
        admit::admit_response(admit::Response::$output(&response)).map_err(|_|Error::Invalid)?;
        $run.check()?;response
    }};
}
impl Run<'_> {
    fn check(&self) -> Result<(), Error> {
        let state = self.owner.0.state.lock().expect("notification admission");
        if state.scope != self.ticket.scope
            || state.fence != self.ticket.fence
            || !state.active
            || !state.foreground
            || state.validated != Some(self.lease.generation)
            || self.stop.is_cancelled()
            || self.lease.cancellation.is_cancelled()
            || self.parent.cancellation.is_cancelled()
            || !self.session.is_current(self.lease.generation)
        {
            Err(Error::Cancelled)
        } else {
            Ok(())
        }
    }
    async fn fresh_parent(&self) -> Result<companion::Installation, Error> {
        self.check()?;
        let api = self.connection.companion();
        let response = tokio::select! {biased;
            ()=self.lease.cancellation.cancelled()=>return Err(Error::Cancelled),
            response=api.get_installation(companion::GetInstallationRequest {
                selector:Some(companion::get_installation_request::Selector::Id(Box::new((*self.parent.installation.id).clone()))),..Default::default()
            },self.lease.token(),Duration::from_secs(5),self.stop)=>response.map_err(Error::Transport)?,
        };
        parent_admit::admit_response(parent_admit::Response::Get(&response))
            .map_err(|_| Error::Invalid)?;
        parent_admit::bind_existing(
            &response.context,
            &response.installation,
            &self.parent.context,
            &self.parent.installation,
        )
        .map_err(|_| Error::Invalid)?;
        parent_admit::bind_native(
            &response.installation,
            self.lease.account_id(),
            self.lease.session_id(),
        )
        .map_err(|_| Error::Invalid)?;
        self.check()?;
        if response.installation.state != companion::InstallationState::Active
            || system(&response.installation.expires_at)
                .is_none_or(|until| until <= SystemTime::now())
        {
            return Err(Error::Parent);
        }
        let mut state = self.owner.0.state.lock().expect("notification parent");
        state.parent = Some((*response.installation).clone());
        Ok((*response.installation).clone())
    }
    async fn settings(&self, request: bool) -> Result<wire::PermissionSnapshot, Error> {
        self.check()?;
        let source = self.owner.0.source.clone();
        let settings = tokio::task::spawn_blocking(move || source.settings(request))
            .await
            .map_err(|_| Error::Invalid)?;
        self.check()?;
        let failure = settings.as_ref().err().copied();
        let value = native::permission_for(settings, self.owner.0.source.backend());
        if !admit::valid_permission(&value) {
            return Err(Error::Invalid);
        }
        let mut state = self.owner.0.state.lock().expect("notification permission");
        if !admit::can_present(&value) {
            state.stop_native();
        }
        state.permission = value.clone();
        if let Some(error) = failure {
            state.notice = error.label().into();
        }
        state.bump();
        self.owner.post(&state);
        Ok(value)
    }
    fn destination(&self) -> Result<wire::Destination, Error> {
        self.owner
            .0
            .state
            .lock()
            .expect("notification destination")
            .destination
            .clone()
            .ok_or(Error::Invalid)
    }
    fn accept_destination(
        &self,
        context: &companion::Context,
        value: wire::Destination,
    ) -> Result<(), Error> {
        self.check()?;
        let mut state = self
            .owner
            .0
            .state
            .lock()
            .expect("notification destination admission");
        if context != &self.parent.context {
            return Err(Error::Invalid);
        }
        admit::bind_native(
            &value,
            state.parent.as_ref().ok_or(Error::Parent)?,
            self.lease.account_id(),
            self.lease.session_id(),
        )
        .map_err(|_| Error::Invalid)?;
        if let Some(prior) = &state.destination {
            // A new destination is admitted only for its explicit registration.
            if prior.id == value.id {
                admit::bind_existing(context, &value, &self.parent.context, prior)
                    .map_err(|_| Error::Invalid)?;
            } else if !matches!(&state.pending, Some(Pending::Register(_))) {
                return Err(Error::Invalid);
            }
        }
        if !value.native_delivery || value.state != wire::DestinationState::Active {
            state.stop_native();
        }
        if value.state != wire::DestinationState::Active {
            state.entries.clear();
            state.grant = None;
            state.local_stopped = true;
        }
        state.stage = match value.state {
            EnumValue::Known(wire::DestinationState::Active) => Stage::Ready,
            EnumValue::Known(wire::DestinationState::Revoked) => Stage::Revoked,
            EnumValue::Known(wire::DestinationState::Expired) => Stage::Expired,
            EnumValue::Known(wire::DestinationState::Unspecified) | EnumValue::Unknown(_) => {
                return Err(Error::Invalid);
            }
        };
        state.context = Some(context.clone());
        state.destination = Some(value);
        state.bump();
        self.owner.post(&state);
        Ok(())
    }
    fn accept_delivery(
        &self,
        context: &companion::Context,
        value: wire::Delivery,
    ) -> Result<(), Error> {
        self.check()?;
        if context != &self.parent.context {
            return Err(Error::Invalid);
        }
        let mut state = self
            .owner
            .0
            .state
            .lock()
            .expect("notification receipt admission");
        let destination = state.destination.as_ref().ok_or(Error::Invalid)?;
        let prior = state.entries.iter().find(|e| e.id == value.id);
        admit::bind_delivery(&value, destination, prior).map_err(|_| Error::Invalid)?;
        if let Some(entry) = state.entries.iter_mut().find(|e| e.id == value.id) {
            *entry = value;
        } else {
            return Err(Error::Invalid);
        }
        state.bump();
        self.owner.post(&state);
        Ok(())
    }
    fn reserve(&self, pending: Pending) -> Result<(), Error> {
        self.check()?;
        let mut state = self.owner.0.state.lock().expect("notification pending");
        if state.pending.is_some() {
            return Err(Error::Invalid);
        }
        if matches!(&pending,Pending::Update(r) if r.native_delivery) {
            state.enable_fence = Some(state.fence);
        }
        state.pending = Some(pending);
        state.resolution_attempted = false;
        state.bump();
        self.owner.post(&state);
        Ok(())
    }
    fn finish_destination(&self, pending: &Pending, value: wire::Destination) -> Result<(), Error> {
        if value.last_request_id != pending.nonce() {
            return Err(Error::Invalid);
        }
        match pending {
            Pending::Register(r) => {
                if value.registration_request_id != r.request_id
                    || value.installation_id != r.installation_id
                    || value.revision != 1
                    || value.native_delivery
                    || value.state != wire::DestinationState::Active
                    || value.permission != r.permission
                {
                    return Err(Error::Invalid);
                }
            }
            Pending::Update(r) => {
                if value.id != r.id
                    || value.revision != r.expected_revision + 1
                    || value.native_delivery != r.native_delivery
                    || value.permission != r.permission
                    || value.state != wire::DestinationState::Active
                {
                    return Err(Error::Invalid);
                }
            }
            Pending::Revoke(r) => {
                if value.id != r.id
                    || value.revision != r.expected_revision + 1
                    || value.native_delivery
                    || value.state != wire::DestinationState::Revoked
                {
                    return Err(Error::Invalid);
                }
            }
            Pending::Receipt(_) => return Err(Error::Invalid),
        }
        if matches!(pending, Pending::Register(_)) {
            let mut state = self
                .owner
                .0
                .state
                .lock()
                .expect("new notification destination");
            state.stop_native();
            state.local_stopped = true;
            state.entries.clear();
            state.inbox_revision = 0;
        }
        self.accept_destination(&self.parent.context, value)?;
        let mut state = self
            .owner
            .0
            .state
            .lock()
            .expect("notification write acknowledged");
        if let Pending::Update(r) = pending {
            if r.native_delivery && state.enable_fence == Some(state.fence) {
                state.local_stopped = false;
            }
            if !r.native_delivery {
                state.local_stopped = true;
            }
        }
        state.pending = None;
        state.resolution_attempted = false;
        state.notice.clear();
        state.bump();
        self.owner.post(&state);
        Ok(())
    }
    fn finish_receipt(
        &self,
        pending: &wire::AcknowledgeDeliveryRequest,
        value: wire::Delivery,
    ) -> Result<(), Error> {
        if value.last_request_id != pending.request_id
            || value.id != pending.delivery_id
            || value.destination_id != pending.destination_id
            || value.revision != pending.expected_revision + 1
        {
            return Err(Error::Invalid);
        }
        let applied = match pending.kind {
            EnumValue::Known(wire::ReceiptKind::Received) => {
                value.reception == wire::Reception::Received
            }
            EnumValue::Known(wire::ReceiptKind::Read) => value.read_state == wire::ReadState::Read,
            EnumValue::Known(wire::ReceiptKind::Dismissed) => {
                value.read_state == wire::ReadState::Dismissed
            }
            EnumValue::Known(wire::ReceiptKind::OsAccepted) => {
                value.presentation == wire::Presentation::OsAccepted
            }
            EnumValue::Known(wire::ReceiptKind::PresentInOsCenter) => {
                value.presentation == wire::Presentation::PresentInOsCenter
            }
            EnumValue::Known(wire::ReceiptKind::OutcomeUnknown) => {
                value.presentation == wire::Presentation::OutcomeUnknown
            }
            EnumValue::Known(wire::ReceiptKind::PermissionDenied) => {
                value.presentation == wire::Presentation::PermissionDenied
            }
            EnumValue::Known(wire::ReceiptKind::Unavailable) => {
                value.presentation == wire::Presentation::Unavailable
            }
            EnumValue::Known(wire::ReceiptKind::Unspecified) | EnumValue::Unknown(_) => false,
        };
        if !applied {
            return Err(Error::Invalid);
        }
        self.accept_delivery(&self.parent.context, value)?;
        let mut state = self
            .owner
            .0
            .state
            .lock()
            .expect("notification receipt acknowledged");
        state.pending = None;
        state.resolution_attempted = false;
        Ok(())
    }
    async fn write(&self, pending: Pending) -> Result<(), Error> {
        self.reserve(pending.clone())?;
        let run = self;
        match &pending {
            Pending::Register(r) => {
                let value = call!(run, register_destination, (**r).clone(), Register, Register);
                self.finish_destination(&pending, (*value.destination).clone())
            }
            Pending::Update(r) => {
                let value = call!(run, update_destination, (**r).clone(), Update, Update);
                self.finish_destination(&pending, (*value.destination).clone())
            }
            Pending::Revoke(r) => {
                let value = call!(run, revoke_destination, (**r).clone(), Revoke, Revoke);
                self.finish_destination(&pending, (*value.destination).clone())
            }
            Pending::Receipt(r) => {
                let value = call!(
                    run,
                    acknowledge_delivery,
                    (**r).clone(),
                    Acknowledge,
                    Acknowledge
                );
                self.finish_receipt(r, (*value.delivery).clone())
            }
        }
    }
    async fn resolve(&self, pending: Pending) -> Result<(), Error> {
        let run = self;
        if let Pending::Receipt(r) = &pending {
            let value = call!(
                run,
                get_delivery,
                wire::GetDeliveryRequest {
                    destination_id: r.destination_id.clone(),
                    delivery_id: r.delivery_id.clone(),
                    ..Default::default()
                },
                Delivery,
                Delivery
            );
            if value.delivery.last_request_id == r.request_id {
                self.finish_receipt(r, (*value.delivery).clone())?;
            } else {
                self.accept_delivery(&value.context, (*value.delivery).clone())?;
                let mut state = self
                    .owner
                    .0
                    .state
                    .lock()
                    .expect("notification receipt not applied");
                state.pending = None;
                state.local_stopped = true;
                state.stop_native();
                state.notice =
                    "Le reçu n’est pas confirmé. Aucun renvoi ni présentation automatique.".into();
            }
        } else {
            let value = call!(
                run,
                get_destination,
                pending.destination_selector(),
                Get,
                Get
            );
            if value.destination.last_request_id == pending.nonce() {
                self.finish_destination(&pending, (*value.destination).clone())?;
            } else {
                self.accept_destination(&value.context, (*value.destination).clone())?;
                let mut state = self
                    .owner
                    .0
                    .state
                    .lock()
                    .expect("notification write not applied");
                state.pending = None;
                state.local_stopped = true;
                state.stop_native();
                state.notice="L’opération n’est pas confirmée. Aucun renvoi automatique ; choisissez une nouvelle action si nécessaire.".into();
            }
        }
        Ok(())
    }
    async fn update(
        &self,
        native_delivery: bool,
        permission: wire::PermissionSnapshot,
    ) -> Result<(), Error> {
        let destination = self.destination()?;
        self.write(Pending::Update(Box::new(wire::UpdateDestinationRequest {
            expected_context: self.parent.context.clone().into(),
            id: destination.id,
            expected_revision: destination.revision,
            request_id: nonce()?,
            native_delivery,
            permission: permission.into(),
            ..Default::default()
        })))
        .await
    }
    async fn receipt(
        &self,
        entry: &wire::Delivery,
        kind: wire::ReceiptKind,
    ) -> Result<wire::Delivery, Error> {
        let destination = self.destination()?;
        self.write(Pending::Receipt(Box::new(
            wire::AcknowledgeDeliveryRequest {
                expected_context: self.parent.context.clone().into(),
                destination_id: destination.id,
                delivery_id: entry.id.clone(),
                expected_revision: entry.revision,
                request_id: nonce()?,
                kind: kind.into(),
                observed_at: now().into(),
                ..Default::default()
            },
        )))
        .await?;
        self.owner
            .0
            .state
            .lock()
            .expect("notification current receipt")
            .entries
            .iter()
            .find(|e| e.id == entry.id)
            .cloned()
            .ok_or(Error::Invalid)
    }
    async fn read(&self) -> Result<(), Error> {
        let parent = self.fresh_parent().await?;
        let pending = self
            .owner
            .0
            .state
            .lock()
            .expect("notification resolution")
            .pending
            .clone();
        if let Some(pending) = pending {
            return self.resolve(pending).await;
        }
        let permission = self.settings(false).await?;
        let prior = self
            .owner
            .0
            .state
            .lock()
            .expect("notification read selector")
            .destination
            .clone();
        let run = self;
        if let Some(prior) = prior {
            let value = call!(
                run,
                get_destination,
                wire::GetDestinationRequest {
                    selector: Some(wire::get_destination_request::Selector::Id(Box::new(
                        (*prior.id).clone()
                    ))),
                    ..Default::default()
                },
                Get,
                Get
            );
            self.accept_destination(&value.context, (*value.destination).clone())?;
            if value.destination.state != wire::DestinationState::Active {
                return Ok(());
            }
            if value.destination.permission.as_option() != Some(&permission) {
                let desired = value.destination.native_delivery
                    && admit::can_present(&permission)
                    && !self
                        .owner
                        .0
                        .state
                        .lock()
                        .expect("notification local consent")
                        .local_stopped;
                self.update(desired, permission.clone()).await?;
            }
            self.native_scope(&parent, &self.destination()?, &permission)?;
            self.inbox().await
        } else {
            let value = call!(
                run,
                describe_notifications,
                wire::DescribeNotificationsRequest::default(),
                Describe,
                Describe
            );
            if value.context.as_option() != Some(&self.parent.context) {
                return Err(Error::Invalid);
            }
            let mut state = self.owner.0.state.lock().expect("notification discovery");
            state.context = Some((*value.context).clone());
            state.stage = Stage::Unregistered;
            state.bump();
            self.owner.post(&state);
            Ok(())
        }
    }
    async fn inbox(&self) -> Result<(), Error> {
        let destination = self.destination()?;
        let run = self;
        let response = call!(
            run,
            read_inbox,
            wire::ReadInboxRequest {
                id: destination.id.clone(),
                ..Default::default()
            },
            Inbox,
            Inbox
        );
        self.accept_destination(&response.context, (*response.destination).clone())?;
        {
            let mut state = self
                .owner
                .0
                .state
                .lock()
                .expect("notification inbox admission");
            if response.revision < state.inbox_revision {
                return Err(Error::Invalid);
            }
            for entry in &response.entries {
                admit::bind_delivery(
                    entry,
                    &response.destination,
                    state.entries.iter().find(|old| old.id == entry.id),
                )
                .map_err(|_| Error::Invalid)?;
            }
            let retired = state
                .entries
                .iter()
                .filter(|prior| !response.entries.iter().any(|next| next.id == prior.id))
                .filter_map(|e| os::PresentationId::parse(&e.id.value).ok())
                .collect::<Vec<_>>();
            for id in retired {
                if let Some(scope) = &state.native {
                    scope.retire(&id);
                }
                if !state.cleanup.contains(&id) {
                    state.cleanup.push(id);
                }
            }
            state.inbox_revision = response.revision;
            state.entries = response.entries;
            state.expire();
            state.bump();
            self.owner.post(&state);
        }
        let ids = self
            .owner
            .0
            .state
            .lock()
            .expect("notification receipt plan")
            .entries
            .iter()
            .map(|e| (*e.id).clone())
            .collect::<Vec<_>>();
        for id in ids {
            self.check()?;
            let Some(mut entry) = self
                .owner
                .0
                .state
                .lock()
                .expect("notification message")
                .entries
                .iter()
                .find(|e| e.id.as_option() == Some(&id))
                .cloned()
            else {
                continue;
            };
            if !state::fresh(&entry, &now()) {
                continue;
            }
            if entry.reception == wire::Reception::NotReceived {
                entry = self.receipt(&entry, wire::ReceiptKind::Received).await?;
            }
            let destination = self.destination()?;
            let local = self
                .owner
                .0
                .state
                .lock()
                .expect("notification native consent")
                .local_stopped;
            if !local && admit::eligible_for_native(&entry, &destination, &now()) {
                self.present(entry).await?;
            } else if !local
                && (entry.presentation == wire::Presentation::OsAccepted
                    || entry.presentation == wire::Presentation::OutcomeUnknown)
            {
                self.evidence(&entry).await?;
            }
        }
        // Native click IDs use exactly the inbox ResolveOpen path.
        let clicked = {
            let mut state = self
                .owner
                .0
                .state
                .lock()
                .expect("notification native click");
            state.pending_native_open(Instant::now()).or_else(|| {
                state
                    .native
                    .as_ref()
                    .and_then(os::PresentationScope::take_open)
                    .and_then(|id| admit::DeliveryId::parse(id.as_str()).ok())
            })
        };
        if let Some(id) = clicked {
            self.open(id).await?;
        }
        Ok(())
    }
    fn native_scope(
        &self,
        parent: &companion::Installation,
        destination: &wire::Destination,
        permission: &wire::PermissionSnapshot,
    ) -> Result<Option<os::PresentationScope>, Error> {
        self.check()?;
        let mut state = self
            .owner
            .0
            .state
            .lock()
            .expect("notification native scope");
        if state.local_stopped || !destination.native_delivery || !admit::can_present(permission) {
            state.stop_native();
            return Ok(None);
        }
        if state
            .native_parent_stop
            .as_ref()
            .is_some_and(CancellationToken::is_cancelled)
        {
            state.stop_native();
        }
        if state.native.is_none() {
            let native = self.lease.cancellation.clone();
            let owner = state.native_stop.clone();
            let parent = self.parent.cancellation.clone();
            state.native_parent_stop = Some(parent.clone());
            let binding = state.open_binding().ok_or(Error::Invalid)?;
            let target = Arc::downgrade(&self.owner.0);
            let on_open = Arc::new(move |id: os::PresentationId| {
                let Some(target) = target.upgrade() else {
                    return;
                };
                let Ok(id) = admit::DeliveryId::parse(id.as_str()) else {
                    return;
                };
                let mut state = target.state.lock().expect("native notification intention");
                let queued = state.capture_native_open(binding.clone(), id, Instant::now());
                drop(state);
                if queued {
                    target.changed.notify_one();
                }
            });
            state.native = Some(os::PresentationScope::with_open_handler(
                move || !native.is_cancelled() && !owner.is_cancelled() && !parent.is_cancelled(),
                on_open,
            ));
        }
        let scope = state.native.as_ref().expect("created native scope").clone();
        let current = SystemTime::now();
        let until = current
            .checked_add(os::MAX_SCOPE_FRESHNESS)
            .ok_or(Error::Invalid)?
            .min(system(&parent.expires_at).ok_or(Error::Invalid)?)
            .min(system(&destination.expires_at).ok_or(Error::Invalid)?);
        scope.refresh_until(until, current).map_err(Error::Native)?;
        Ok(Some(scope))
    }
    async fn present(&self, entry: wire::Delivery) -> Result<(), Error> {
        // Refresh both grants and OS settings before reserving this one attempt.
        let parent = self.fresh_parent().await?;
        let permission = self.settings(false).await?;
        let prior = self.destination()?;
        let run = self;
        let response = call!(
            run,
            get_destination,
            wire::GetDestinationRequest {
                selector: Some(wire::get_destination_request::Selector::Id(Box::new(
                    (*prior.id).clone()
                ))),
                ..Default::default()
            },
            Get,
            Get
        );
        self.accept_destination(&response.context, (*response.destination).clone())?;
        let destination = self.destination()?;
        if !admit::can_present(&permission)
            || !admit::eligible_for_native(&entry, &destination, &now())
        {
            return Ok(());
        }
        let Some(scope) = self.native_scope(&parent, &destination, &permission)? else {
            return Ok(());
        };
        let current = SystemTime::now();
        let content = os::Content::new(
            os::PresentationId::parse(&entry.id.value).map_err(Error::Native)?,
            entry.title.clone(),
            entry.body.clone(),
            system(&entry.created_at).ok_or(Error::Invalid)?,
            system(&entry.expires_at).ok_or(Error::Invalid)?,
            current,
        )
        .map_err(Error::Native)?;
        // Durable server reservation precedes any possible native submission.
        let entry = self
            .receipt(&entry, wire::ReceiptKind::OutcomeUnknown)
            .await?;
        self.check()?;
        let source = self.owner.0.source.clone();
        let native_scope = scope.clone();
        let result = tokio::task::spawn_blocking(move || source.present(&native_scope, &content))
            .await
            .map_err(|_| Error::Invalid)?;
        self.check()?;
        match result {
            Ok(os::Submission::AcceptedByOs) => {
                let entry = self.receipt(&entry, wire::ReceiptKind::OsAccepted).await?;
                self.evidence(&entry).await?;
            }
            Err(error) => {
                let mut state = self.owner.0.state.lock().expect("notification OS result");
                state.notice = error.label().into();
                if error == os::Error::ScopeClosed || error == os::Error::Capacity {
                    state.stop_native();
                }
                state.bump();
                self.owner.post(&state);
            }
        }
        Ok(())
    }
    async fn evidence(&self, entry: &wire::Delivery) -> Result<(), Error> {
        self.check()?;
        if !state::fresh(entry, &now()) {
            return Ok(());
        }
        let id = os::PresentationId::parse(&entry.id.value).map_err(Error::Native)?;
        let source = self.owner.0.source.clone();
        let evidence = tokio::task::spawn_blocking(move || source.evidence(&id))
            .await
            .map_err(|_| Error::Invalid)?;
        self.check()?;
        if evidence.is_ok_and(os::DeliveryEvidence::attests_center) {
            self.receipt(entry, wire::ReceiptKind::PresentInOsCenter)
                .await?;
        }
        Ok(())
    }
    async fn open(&self, id: admit::DeliveryId) -> Result<(), Error> {
        self.fresh_parent().await?;
        let destination = self.destination()?;
        let run = self;
        let response = call!(
            run,
            get_delivery,
            wire::GetDeliveryRequest {
                destination_id: destination.id.clone(),
                delivery_id: wire::DeliveryId {
                    value: id.as_str().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
            Delivery,
            Delivery
        );
        self.accept_delivery(&response.context, (*response.delivery).clone())?;
        let message = (*response.delivery).clone();
        let grant = call!(
            run,
            resolve_open,
            wire::ResolveOpenRequest {
                destination_id: destination.id.clone(),
                delivery_id: message.id.clone(),
                ..Default::default()
            },
            Open,
            Open
        );
        admit::bind_open(&grant, &self.parent.context, &destination, &message, &now()).map_err(
            |error| {
                #[cfg(debug_assertions)]
                eprintln!("homeward: notification grant rejected {error:?}");
                Error::Invalid
            },
        )?;
        #[cfg(debug_assertions)]
        eprintln!("homeward: notification grant ready");
        let mut state = self.owner.0.state.lock().expect("notification open grant");
        state.next_grant = state.next_grant.checked_add(1).ok_or(Error::Invalid)?;
        let generation = state.next_grant;
        state.grant = Some(Grant {
            tag: generation,
            response: grant,
            message,
            parent_stop: self.parent.cancellation.clone(),
            native_generation: self.lease.generation,
        });
        state.bump();
        self.owner.post(&state);
        drop(state);
        let scope = self.ticket.scope;
        if let Some(window) = &self.owner.0.window {
            let owner = self.owner.clone();
            let _ = window.upgrade_in_event_loop(move |window| {
                let state = owner.0.state.lock().expect("notification open dispatch");
                if state.scope != scope
                    || state
                        .grant
                        .as_ref()
                        .is_none_or(|grant| grant.tag != generation)
                    || !owner.authenticated(&state)
                {
                    #[cfg(debug_assertions)]
                    eprintln!("homeward: notification grant dispatch invalidated");
                    return;
                }
                drop(state);
                #[cfg(debug_assertions)]
                eprintln!("homeward: notification grant dispatching");
                window.invoke_notification_target(generation);
            });
        }
        Ok(())
    }
    async fn intent(&self, intent: Intent) -> Result<(), Error> {
        self.fresh_parent().await?;
        match intent {
            Intent::Register => {
                let permission = self.settings(false).await?;
                self.write(Pending::Register(Box::new(
                    wire::RegisterDestinationRequest {
                        expected_context: self.parent.context.clone().into(),
                        installation_id: self.parent.installation.id.clone(),
                        request_id: nonce()?,
                        permission: permission.into(),
                        ..Default::default()
                    },
                )))
                .await
            }
            Intent::Permission => {
                self.settings(true).await?;
                Ok(())
            }
            Intent::Enable => {
                let permission = self.settings(false).await?;
                if !admit::can_present(&permission) {
                    return Err(Error::Native(os::Error::PermissionDenied));
                }
                self.update(true, permission).await
            }
            Intent::Disable => {
                if self
                    .owner
                    .0
                    .state
                    .lock()
                    .expect("notification disable")
                    .destination
                    .as_ref()
                    .is_some_and(|d| d.state == wire::DestinationState::Active)
                {
                    let permission = self.settings(false).await?;
                    self.update(false, permission).await
                } else {
                    Ok(())
                }
            }
            Intent::Revoke => {
                let destination = self.destination()?;
                self.write(Pending::Revoke(Box::new(wire::RevokeDestinationRequest {
                    expected_context: self.parent.context.clone().into(),
                    id: destination.id,
                    expected_revision: destination.revision,
                    request_id: nonce()?,
                    ..Default::default()
                })))
                .await
            }
            Intent::Row(id, action) => match action {
                NotificationRowAction::Open => self.open(id).await,
                NotificationRowAction::Read | NotificationRowAction::Dismiss => {
                    let entry = self
                        .owner
                        .0
                        .state
                        .lock()
                        .expect("notification row")
                        .entries
                        .iter()
                        .find(|e| e.id.value == id.as_str())
                        .cloned()
                        .ok_or(Error::Invalid)?;
                    let kind = match action {
                        NotificationRowAction::Read => wire::ReceiptKind::Read,
                        NotificationRowAction::Dismiss => wire::ReceiptKind::Dismissed,
                        NotificationRowAction::Open => unreachable!("open handled separately"),
                    };
                    let result = self.receipt(&entry, kind).await?;
                    if result.read_state == wire::ReadState::Dismissed {
                        let id =
                            os::PresentationId::parse(&result.id.value).map_err(Error::Native)?;
                        let mut state = self.owner.0.state.lock().expect("notification dismiss");
                        if let Some(scope) = &state.native {
                            scope.retire(&id);
                        }
                        state.cleanup.push(id);
                        state.expire();
                    }
                    Ok(())
                }
            },
        }
    }
}
impl Notifications {
    pub(super) async fn work(
        &self,
        ticket: Ticket,
        session: SessionCoordinator,
        connection: Connection,
        parent: Option<crate::companion_host::NotificationParent>,
        stop: CancellationToken,
        job: Job,
    ) {
        if let Job::Cleanup(ids) = job {
            self.cleanup(ids).await;
            let mut state = self.0.state.lock().expect("notification cleaned");
            if state.scope == ticket.scope {
                state.running = false;
            }
            return;
        }
        let result = self
            .execute(ticket, &session, &connection, parent.as_ref(), &stop, job)
            .await;
        let mut state = self.0.state.lock().expect("notification work complete");
        if state.scope != ticket.scope {
            return;
        }
        state.running = false;
        state.next_poll = Instant::now() + Duration::from_secs(admit::MINIMUM_POLL_SECONDS as u64);
        if state.fence != ticket.fence {
            state.requested |= state.pending.is_some();
            self.post(&state);
            return;
        }
        match result {
            Ok(()) => {
                if state.pending.is_none()
                    && state
                        .destination
                        .as_ref()
                        .is_some_and(|d| d.state == wire::DestinationState::Active)
                {
                    state.stage = Stage::Ready;
                }
            }
            Err(error) => {
                state.stop_native();
                state.grant = None;
                match error {
                    Error::Cancelled => {
                        state.requested |= state.pending.is_some();
                    }
                    Error::Invalid => state.invalid(),
                    Error::Parent => {
                        state.entries.clear();
                        state.parent = None;
                        state.stage = Stage::WaitingParent;
                        state.intent = None;
                    }
                    Error::Native(error) => {
                        state.notice = error.label().into();
                    }
                    Error::Transport(error) => {
                        state.intent = None;
                        if state.pending.is_some() {
                            if error.kind() == FailureKind::Rpc(RpcCode::NotFound)
                                && matches!(&state.pending, Some(Pending::Receipt(_)))
                            {
                                if let Some(Pending::Receipt(request)) = state.pending.take() {
                                    state
                                        .entries
                                        .retain(|entry| entry.id != request.delivery_id);
                                    if let Ok(id) =
                                        os::PresentationId::parse(&request.delivery_id.value)
                                    {
                                        state.cleanup.push(id);
                                    }
                                }
                                state.notice =
                                    "Message expiré ou retiré ; aucun reçu renvoyé.".into();
                                state.requested = false;
                                state.stage = Stage::Ready;
                            } else if error.kind() == FailureKind::Rpc(RpcCode::NotFound)
                                && matches!(&state.pending, Some(Pending::Register(_)))
                            {
                                state.pending = None;
                                state.destination = None;
                                state.stage = Stage::Unregistered;
                                state.local_stopped = true;
                                state.notice =
                                    "Aucune inscription retrouvée. Aucun renvoi automatique."
                                        .into();
                            } else {
                                state.stage = Stage::Uncertain;
                                state.requested = !state.resolution_attempted;
                                state.notice="Résultat incertain : résolution par lecture, sans renvoi de mutation.".into();
                            }
                        } else {
                            state.entries.clear();
                            state.stage = match error.kind() {
                                FailureKind::Rpc(
                                    RpcCode::Unauthenticated | RpcCode::PermissionDenied,
                                ) => Stage::Denied,
                                FailureKind::Rpc(RpcCode::Unimplemented) => Stage::Unsupported,
                                FailureKind::Rpc(RpcCode::NotFound) => Stage::Revoked,
                                FailureKind::InvalidConfiguration
                                | FailureKind::UntrustedCertificate
                                | FailureKind::WrongServerName
                                | FailureKind::TlsRejected => Stage::Invalid,
                                FailureKind::NetworkInterrupted
                                | FailureKind::Deadline
                                | FailureKind::Cancelled
                                | FailureKind::Rpc(_) => Stage::Offline,
                            };
                        }
                    }
                }
            }
        }
        state.bump();
        self.post(&state);
    }
    async fn execute(
        &self,
        ticket: Ticket,
        session: &SessionCoordinator,
        connection: &Connection,
        parent: Option<&crate::companion_host::NotificationParent>,
        stop: &CancellationToken,
        job: Job,
    ) -> Result<(), Error> {
        let parent = parent.ok_or(Error::Parent)?;
        let lease = session.access(stop).await.map_err(|_| Error::Cancelled)?;
        {
            let mut state = self.0.state.lock().expect("notification native binding");
            if state.scope != ticket.scope || state.fence != ticket.fence {
                return Err(Error::Cancelled);
            }
            let identity = (lease.account_id().clone(), lease.session_id().clone());
            if state.identity.as_ref().is_some_and(|old| old != &identity) {
                state.clear();
                return Err(Error::Cancelled);
            }
            state.identity = Some(identity);
            state.validated = Some(lease.generation);
        }
        let run = Run {
            owner: self,
            ticket,
            session,
            connection,
            lease: &lease,
            stop,
            parent,
        };
        let result = match job {
            Job::Read => run.read().await,
            Job::Intent(intent) => run.intent(intent).await,
            Job::Cleanup(_) => unreachable!("cleanup is handled without credentials"),
        };
        if let Err(Error::Transport(error)) = &result {
            let _ = session.observe_failure(lease.generation, error).await;
        }
        result
    }
}
