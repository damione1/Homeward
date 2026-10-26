//! Single-attempt ListService calls on the shared verified TLS connection.
//! The host owns revisions, admission, drafts and explicit conflict resolution.
use crate::{AccessToken, Failure, StateTransport, services::home::lists::v1 as service};
use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::lists::v1 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct ListTransport {
    pub(crate) client: service::ListServiceClient<HttpClient>,
}

macro_rules! unary {
    ($name:ident, $method:ident, $request:ident, $response:ident) => {
        pub async fn $name(
            &self,
            request: wire::$request,
            token: &AccessToken,
            timeout: Duration,
            cancel: &CancellationToken,
        ) -> Result<wire::$response, Failure> {
            let options = StateTransport::options(token, timeout)?;
            tokio::select! { biased;
                () = cancel.cancelled() => Err(Failure::cancelled()),
                response = self.client.$method(request, options) => {
                    response.map(|value| value.into_owned()).map_err(Failure::from)
                }
            }
        }
    };
}
impl ListTransport {
    unary!(
        list_lists,
        list_lists_with_options,
        ListListsRequest,
        ListListsResponse
    );
    unary!(
        count_lists,
        count_lists_with_options,
        CountListsRequest,
        CountListsResponse
    );
    unary!(
        get_list,
        get_list_with_options,
        GetListRequest,
        GetListResponse
    );
    unary!(
        create_list,
        create_list_with_options,
        CreateListRequest,
        CreateListResponse
    );
    unary!(
        rename_list,
        rename_list_with_options,
        RenameListRequest,
        RenameListResponse
    );
    unary!(
        delete_list,
        delete_list_with_options,
        DeleteListRequest,
        DeleteListResponse
    );
    unary!(
        list_items,
        list_items_with_options,
        ListItemsRequest,
        ListItemsResponse
    );
    unary!(
        count_items,
        count_items_with_options,
        CountItemsRequest,
        CountItemsResponse
    );
    unary!(
        get_item,
        get_item_with_options,
        GetItemRequest,
        GetItemResponse
    );
    unary!(
        add_item,
        add_item_with_options,
        AddItemRequest,
        AddItemResponse
    );
    unary!(
        edit_item,
        edit_item_with_options,
        EditItemRequest,
        EditItemResponse
    );
    unary!(
        delete_item,
        delete_item_with_options,
        DeleteItemRequest,
        DeleteItemResponse
    );
    unary!(
        set_completed,
        set_completed_with_options,
        SetCompletedRequest,
        SetCompletedResponse
    );
    unary!(
        move_item,
        move_item_with_options,
        MoveItemRequest,
        MoveItemResponse
    );

    /// Ready establishes the subscription; the host then reloads the page
    /// while keeping this stream alive. The protocol has no replay cursor.
    pub async fn watch_lists(
        &self,
        request: wire::WatchListsRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<ListWatch, Failure> {
        let options = StateTransport::options(token, timeout)?;
        tokio::select! { biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.watch_lists_with_options(request, options) => {
                response.map(|inner| ListWatch { inner: Some(inner), cancel: cancel.child_token() }).map_err(Failure::from)
            }
        }
    }
}

type WireStream = ServerStream<
    <HttpClient as ClientTransport>::ResponseBody,
    wire::__buffa::view::WatchListsResponseView<'static>,
>;
pub struct ListWatch {
    inner: Option<WireStream>,
    cancel: CancellationToken,
}
impl ListWatch {
    pub async fn next(&mut self) -> Result<Option<wire::WatchListsResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! { biased;
            () = self.cancel.cancelled() => Err(Failure::cancelled()),
            response = inner.message() => response.map(|value|value.map(|frame|frame.to_owned_message())).map_err(Failure::from),
        };
        if !matches!(result, Ok(Some(_))) {
            self.inner = None;
        }
        result
    }
}
impl Drop for ListWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
