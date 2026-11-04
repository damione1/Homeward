//! Single-attempt TaskService calls on the shared verified TLS connection.
//! The host owns revisions, admission, drafts and explicit conflict resolution.
use crate::{AccessToken, Failure, services::home::api::v2 as service};
use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::api::v2 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct TaskTransport {
    pub(crate) homes: crate::HomeTransport,
    pub(crate) client: service::TaskServiceClient<HttpClient>,
}

macro_rules! unary {
    ($name:ident, $method:ident, $request:ident, $response:ident) => {
        pub async fn $name(
            &self,
            request: wire::$request,
            token: &AccessToken,
            timeout: Duration,
            cancel: &CancellationToken,
        ) -> Result<responses::$response, Failure> {
            let options = token.options(timeout)?;
            tokio::select! { biased;
                () = cancel.cancelled() => Err(Failure::cancelled()),
                response = self.client.$method(request, options) => {
                    response.map(|value| value.into_owned()).map_err(Failure::from)
                }
            }
        }
    };
}
mod responses {
    pub use buffa_types::google::protobuf::Empty;
    pub use home_wire_contract::home::api::v2::*;
}
impl TaskTransport {
    pub fn home(&self) -> &crate::HomeTransport {
        &self.homes
    }
    unary!(
        list_task_lists,
        list_task_lists_with_options,
        ListTaskListsRequest,
        ListTaskListsResponse
    );
    unary!(
        get_task_list,
        get_task_list_with_options,
        GetTaskListRequest,
        TaskList
    );
    unary!(
        create_task_list,
        create_task_list_with_options,
        CreateTaskListRequest,
        TaskList
    );
    unary!(
        update_task_list,
        update_task_list_with_options,
        UpdateTaskListRequest,
        TaskList
    );
    unary!(
        delete_task_list,
        delete_task_list_with_options,
        DeleteTaskListRequest,
        Empty
    );
    unary!(
        list_tasks,
        list_tasks_with_options,
        ListTasksRequest,
        ListTasksResponse
    );
    unary!(get_task, get_task_with_options, GetTaskRequest, Task);
    unary!(
        append_task,
        append_task_with_options,
        AppendTaskRequest,
        AppendTaskResponse
    );
    unary!(
        update_task,
        update_task_with_options,
        UpdateTaskRequest,
        Task
    );
    unary!(
        delete_task,
        delete_task_with_options,
        DeleteTaskRequest,
        Empty
    );
    unary!(move_task, move_task_with_options, MoveTaskRequest, Task);
    /// Ready establishes the subscription; the host then reloads the page
    /// while keeping this stream alive. The protocol has no replay cursor.
    pub async fn watch_task_lists(
        &self,
        request: wire::WatchTaskListsRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<TaskWatch, Failure> {
        let options = token.options(timeout)?;
        tokio::select! { biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.watch_task_lists_with_options(request, options) => {
                response.map(|inner| TaskWatch { inner: Some(inner), cancel: cancel.child_token() }).map_err(Failure::from)
            }
        }
    }
}

type WireStream = ServerStream<
    <HttpClient as ClientTransport>::ResponseBody,
    wire::__buffa::view::WatchTaskListsResponseView<'static>,
>;
pub struct TaskWatch {
    inner: Option<WireStream>,
    cancel: CancellationToken,
}
impl TaskWatch {
    pub async fn next(&mut self) -> Result<Option<wire::WatchTaskListsResponse>, Failure> {
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
impl Drop for TaskWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
