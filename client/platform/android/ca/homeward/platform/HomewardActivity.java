package ca.homeward.platform;

import android.Manifest;
import android.app.NativeActivity;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.os.Build;
import android.os.Bundle;
import android.service.notification.StatusBarNotification;
import java.util.ArrayDeque;

/** OS adapter only. Opaque click IDs confer no session or resource authority. */
public final class HomewardActivity extends NativeActivity {
    private static final String CHANNEL = "homeward.messages.v1";
    private static final String CLICK = "ca.homeward.presentation";
    private static final String PREFERENCES = "homeward.notification-permission.v1";
    private static final int REQUEST = 2301;
    private final ArrayDeque<String> pendingOpens = new ArrayDeque<>();
    private enum Permission {
        NotRequested(1), Denied(2), Authorized(3);
        final int code;
        Permission(int code) { this.code = code; }
    }
    private enum Surface {
        Enabled(1), Disabled(2), Unknown(4);
        final int code;
        Surface(int code) { this.code = code; }
    }
    private boolean nativeReady;
    private boolean permissionPending;
    private native void notificationOpened(String opaqueId);

    @Override protected void onCreate(Bundle saved) {
        super.onCreate(saved);
        acceptIntent(getIntent());
    }
    @Override protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        acceptIntent(intent);
    }
    @Override protected void onDestroy() {
        nativeReady = false;
        pendingOpens.clear();
        super.onDestroy();
    }
    private void acceptIntent(Intent intent) {
        if (intent == null) return;
        String id = intent.getStringExtra(CLICK);
        intent.removeExtra(CLICK);
        if (id == null || !id.matches("[0-9a-f]{32}")) return;
        if (nativeReady) notificationOpened(id);
        else if (pendingOpens.size() < 64) pendingOpens.addLast(id);
    }
    public void attachNotifications() {
        runOnUiThread(() -> {
            if (isDestroyed() || isFinishing()) return;
            nativeReady = true;
            while (!pendingOpens.isEmpty()) notificationOpened(pendingOpens.removeFirst());
        });
    }
    private NotificationManager manager() {
        return (NotificationManager) getApplicationContext().getSystemService(NOTIFICATION_SERVICE);
    }
    /** No channel creation, permission prompt, or notification submission. */
    public int[] notificationSettings() {
        NotificationManager manager = manager();
        boolean granted = Build.VERSION.SDK_INT < 33 || checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED;
        boolean asked = getSharedPreferences(PREFERENCES, MODE_PRIVATE).getBoolean("asked", false);
        Permission permission = !granted ? (asked ? Permission.Denied : Permission.NotRequested) : (manager.areNotificationsEnabled() ? Permission.Authorized : Permission.Denied);
        NotificationChannel channel = manager.getNotificationChannel(CHANNEL);
        int importance = channel == null ? -1 : channel.getImportance();
        Surface center = importance < 0 ? Surface.Unknown : (permission == Permission.Authorized && importance > 0 ? Surface.Enabled : Surface.Disabled);
        Surface alerts = importance < 0 ? Surface.Unknown : (permission == Permission.Authorized && importance >= NotificationManager.IMPORTANCE_HIGH ? Surface.Enabled : Surface.Disabled);
        return new int[] {permission.code, alerts.code, center.code};
    }
    /** Only invoked after the application's explicit permission confirmation. */
    public void requestNotifications() {
        runOnUiThread(() -> {
            if (isDestroyed() || isFinishing() || !hasWindowFocus() || permissionPending) return;
            NotificationChannel channel = new NotificationChannel(CHANNEL, "Messages Homeward", NotificationManager.IMPORTANCE_DEFAULT);
            channel.setDescription("Messages autorisés de votre serveur Homeward");
            channel.setSound(null, null);
            channel.enableVibration(false);
            channel.setLockscreenVisibility(Notification.VISIBILITY_PRIVATE);
            manager().createNotificationChannel(channel);
            if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
                permissionPending = true;
                requestPermissions(new String[] {Manifest.permission.POST_NOTIFICATIONS}, REQUEST);
            }
        });
    }
    @Override public void onRequestPermissionsResult(int request, String[] permissions, int[] results) {
        super.onRequestPermissionsResult(request, permissions, results);
        if (request == REQUEST) {
            permissionPending = false;
            if (results.length > 0) getSharedPreferences(PREFERENCES, MODE_PRIVATE).edit().putBoolean("asked", true).apply();
        }
    }
    public void postNotification(String id, String title, String body, long lifetimeMillis) {
        if (isDestroyed() || isFinishing() || lifetimeMillis <= 0) throw new IllegalStateException("Activity no longer admits a notification");
        Intent intent = new Intent(this, HomewardActivity.class);
        intent.setAction("ca.homeward.OPEN." + id);
        intent.putExtra(CLICK, id);
        intent.addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP | Intent.FLAG_ACTIVITY_SINGLE_TOP);
        PendingIntent open = PendingIntent.getActivity(this, 0, intent, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_ONE_SHOT);
        Notification notification = new Notification.Builder(this, CHANNEL)
            .setSmallIcon(android.R.drawable.ic_dialog_info)
            .setContentTitle(title).setContentText(body)
            .setStyle(new Notification.BigTextStyle().bigText(body))
            .setContentIntent(open).setAutoCancel(true)
            .setVisibility(Notification.VISIBILITY_PRIVATE).setOnlyAlertOnce(true)
            .setTimeoutAfter(lifetimeMillis).build();
        manager().notify("homeward." + id, 1, notification);
    }
    public int notificationEvidence(String id) {
        StatusBarNotification[] items = manager().getActiveNotifications();
        if (items.length > 512) return 0;
        String expected = "homeward." + id;
        for (StatusBarNotification item : items) {
            if (item.getId() == 1 && expected.equals(item.getTag())) return 1;
        }
        return 2;
    }
    public void removeNotification(String id) {
        manager().cancel("homeward." + id, 1);
    }
}
