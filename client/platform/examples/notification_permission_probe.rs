//! Read-only: never asks permission, submits a message or removes a notification.
fn main() {
    let settings = home_client_platform::notifications::settings();
    println!(
        "{}",
        serde_json::json!({
            "operation": "read-native-notification-settings-only",
            "result": format!("{settings:?}"),
            "permission_requested": false,
            "message_submitted": false,
            "notification_removed": false
        })
    );
}
