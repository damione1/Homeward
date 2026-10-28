package ca.homeward.qualification;

import android.app.Activity;
import android.app.Instrumentation;
import android.app.NotificationManager;
import android.content.Intent;
import android.os.Bundle;
import android.os.SystemClock;
import ca.homeward.platform.HomewardActivity;

/** OS adapter evidence only: no server, session, delivery receipt or device driver. */
public final class NotificationAdapterProbe extends Instrumentation {
    private enum Stage { READ_ONLY_PASS, AWAITING_EXPLICIT_OS_PERMISSION, OS_REGISTRATION_PASS }
    private static final String FIRST = "0123456789abcdef0123456789abcdef";
    private static final String SECOND = "1123456789abcdef0123456789abcdef";
    @Override public void onCreate(Bundle args) { super.onCreate(args); start(); }
    private void require(boolean condition, String message) {
        if (!condition) throw new AssertionError(message);
    }
    private boolean evidence(HomewardActivity activity, String id, int expected) {
        long until = SystemClock.elapsedRealtime() + 3000;
        do {
            if (activity.notificationEvidence(id) == expected) return true;
            SystemClock.sleep(50);
        } while (SystemClock.elapsedRealtime() < until);
        return false;
    }
    private void stage(Stage value) {
        Bundle report = new Bundle(); report.putString("stage", value.name()); sendStatus(1, report);
    }
    @Override public void onStart() {
        HomewardActivity activity = null;
        Bundle result = new Bundle();
        try {
            Intent launch = new Intent(Intent.ACTION_MAIN);
            launch.setClassName(getTargetContext().getPackageName(), "ca.homeward.platform.HomewardActivity");
            launch.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            activity = (HomewardActivity) startActivitySync(launch);
            waitForIdleSync();
            long until = SystemClock.elapsedRealtime() + 10000;
            while (!activity.hasWindowFocus() && SystemClock.elapsedRealtime() < until) SystemClock.sleep(100);
            require(activity.hasWindowFocus(), "target Activity has no focus");
            NotificationManager manager = (NotificationManager) getTargetContext().getSystemService(Activity.NOTIFICATION_SERVICE);
            require(manager.getNotificationChannel("homeward.messages.v1") == null, "fresh app unexpectedly has a channel");
            int[] before = activity.notificationSettings();
            require(before[0] == 1 && before[1] == 4 && before[2] == 4, "read-only settings invented permission/channel");
            require(manager.getNotificationChannel("homeward.messages.v1") == null, "query created a channel");
            require(activity.notificationEvidence(FIRST) == 2, "unposted message observed");
            stage(Stage.READ_ONLY_PASS);
            activity.requestNotifications();
            stage(Stage.AWAITING_EXPLICIT_OS_PERMISSION);
            until = SystemClock.elapsedRealtime() + 45000;
            int[] settings = activity.notificationSettings();
            while (settings[0] != 3 && SystemClock.elapsedRealtime() < until) {
                SystemClock.sleep(100); settings = activity.notificationSettings();
            }
            require(settings[0] == 3 && settings[1] == 2 && settings[2] == 1, "actual OS permission/channel not enabled");
            until = SystemClock.elapsedRealtime() + 10000;
            while (!activity.hasWindowFocus() && SystemClock.elapsedRealtime() < until) SystemClock.sleep(100);
            require(activity.hasWindowFocus(), "target Activity has not resumed after permission");
            activity.postNotification(FIRST, "Qualification Homeward", "Message synthétique A", 30000);
            activity.postNotification(SECOND, "Qualification Homeward", "Message synthétique B", 30000);
            require(evidence(activity, FIRST, 1) && evidence(activity, SECOND, 1), "OS manager did not register the two messages");
            stage(Stage.OS_REGISTRATION_PASS);
            SystemClock.sleep(2500);
            activity.removeNotification(FIRST);
            require(evidence(activity, FIRST, 2), "targeted removal failed");
            require(activity.notificationEvidence(SECOND) == 1, "targeted removal affected another message");
            activity.removeNotification(SECOND);
            require(evidence(activity, SECOND, 2), "second cleanup failed");
            // Exercise the registered native click callback. No scope exists,
            // so even a correctly shaped opaque ID must convey no authority.
            Intent click = new Intent(getTargetContext(), HomewardActivity.class);
            click.putExtra("ca.homeward.presentation", FIRST);
            click.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_SINGLE_TOP);
            getTargetContext().startActivity(click);
            SystemClock.sleep(500);
            result.putString("result", "PASS_OS_ADAPTER_ONLY");
            finish(Activity.RESULT_OK, result);
        } catch (Throwable failure) {
            result.putString("failure", failure.toString());
            finish(Activity.RESULT_CANCELED, result);
        } finally {
            if (activity != null) {
                activity.removeNotification(FIRST); activity.removeNotification(SECOND);
            }
        }
    }
}
