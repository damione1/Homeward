package ca.homeward.platform;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.content.Context;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.os.SystemClock;

/** OS lifetime only. The existing Rust session owns all TLS and admission. */
public final class HomewardReceptionService extends Service {
    static final String START = "ca.homeward.reception.START";
    static final String STOP = "ca.homeward.reception.STOP";
    static final String OWNER = "owner";
    private static final String CHANNEL = "homeward.reception.v1";
    private static final int NOTIFICATION = 2;
    private static final long IDLE_LIMIT = 15_000;
    private static long owner;
    private static long authorized;
    private static long renewed;
    private static boolean running;
    private final Handler handler = new Handler(Looper.getMainLooper());
    private long mine;
    private enum Action {
        Start, Stop, Invalid;
        static Action parse(Intent intent) {
            if (intent == null) return Invalid;
            if (START.equals(intent.getAction())) return Start;
            if (STOP.equals(intent.getAction())) return Stop;
            return Invalid;
        }
    }
    static synchronized void authorize(long value) { authorized = value; }
    static void revoke(Context context, long value) {
        boolean stop;
        synchronized (HomewardReceptionService.class) {
            stop = owner == value || authorized == value;
            if (authorized == value) authorized = 0;
            if (owner == value) running = false;
        }
        if (stop) HomewardActivity.receptionChanged(value, false);
        if (stop) context.stopService(new Intent(context, HomewardReceptionService.class));
    }
    static void stopAll(Context context) {
        long previous;
        synchronized (HomewardReceptionService.class) { previous = owner; authorized = 0; running = false; }
        if (previous > 0) HomewardActivity.receptionChanged(previous, false);
        context.stopService(new Intent(context, HomewardReceptionService.class));
    }
    static synchronized boolean running(long expected) {
        return running && owner == expected && authorized == expected && SystemClock.elapsedRealtime() - renewed < IDLE_LIMIT;
    }
    static synchronized boolean active() {
        return running && authorized == owner && SystemClock.elapsedRealtime() - renewed < IDLE_LIMIT;
    }
    static synchronized boolean renew(long expected) {
        if (!running(expected)) return false;
        renewed = SystemClock.elapsedRealtime();
        return true;
    }
    private final Runnable watchdog = new Runnable() {
        @Override public void run() {
            if (!running(mine)) { stopSelf(); return; }
            handler.postDelayed(this, 1_000);
        }
    };
    @Override public IBinder onBind(Intent intent) { return null; }
    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        Action action = Action.parse(intent);
        long requested = intent == null ? 0 : intent.getLongExtra(OWNER, 0);
        switch (action) {
            case Start:
                synchronized (HomewardReceptionService.class) {
                    if (mine != 0) {
                        HomewardActivity.receptionChanged(requested, false);
                        return START_NOT_STICKY;
                    }
                    if (requested <= 0 || authorized != requested) {
                        HomewardActivity.receptionChanged(requested, false);
                        stopSelf();
                        return START_NOT_STICKY;
                    }
                }
                mine = requested;
                try {
                    NotificationManager manager = getSystemService(NotificationManager.class);
                    if (!manager.areNotificationsEnabled()) throw new IllegalStateException("Notifications unavailable");
                    NotificationChannel channel = new NotificationChannel(CHANNEL, "Réception en arrière-plan", NotificationManager.IMPORTANCE_LOW);
                    channel.setDescription("Réception locale activée explicitement, avec consommation réseau et batterie.");
                    channel.setSound(null, null);
                    channel.enableVibration(false);
                    channel.setLockscreenVisibility(Notification.VISIBILITY_PRIVATE);
                    manager.createNotificationChannel(channel);
                    if (manager.getNotificationChannel(CHANNEL).getImportance() == NotificationManager.IMPORTANCE_NONE)
                        throw new IllegalStateException("Reception channel unavailable");
                    Intent stop = new Intent(this, HomewardReceptionService.class).setAction(STOP).putExtra(OWNER, mine);
                    PendingIntent stopIntent = PendingIntent.getService(this, 0, stop, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
                    Intent open = new Intent(this, HomewardActivity.class).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP);
                    PendingIntent openIntent = PendingIntent.getActivity(this, 1, open, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
                    Notification notification = new Notification.Builder(this, CHANNEL)
                        .setSmallIcon(android.R.drawable.ic_dialog_info).setContentTitle("Réception Homeward active")
                        .setContentText("Connexion locale · arrêt possible à tout moment")
                        .setContentIntent(openIntent).setOngoing(true).setOnlyAlertOnce(true)
                        .setVisibility(Notification.VISIBILITY_PRIVATE)
                        .addAction(new Notification.Action.Builder(null, "Arrêter l’arrière-plan", stopIntent).build()).build();
                    if (Build.VERSION.SDK_INT >= 34)
                        startForeground(NOTIFICATION, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
                    else startForeground(NOTIFICATION, notification);
                    synchronized (HomewardReceptionService.class) {
                        owner = mine; renewed = SystemClock.elapsedRealtime(); running = true;
                    }
                    HomewardActivity.receptionChanged(mine, true);
                    handler.postDelayed(watchdog, 1_000);
                } catch (RuntimeException refused) {
                    HomewardActivity.receptionChanged(mine, false);
                    stopSelf();
                }
                return START_NOT_STICKY;
            case Stop:
                if (requested == mine) { revoke(this, mine); stopSelf(); }
                return START_NOT_STICKY;
            case Invalid:
                stopSelf();
                return START_NOT_STICKY;
        }
        throw new AssertionError("Exhaustive service action");
    }
    @Override public void onTaskRemoved(Intent root) { stopSelf(); }
    @Override public void onDestroy() {
        handler.removeCallbacks(watchdog);
        synchronized (HomewardReceptionService.class) {
            if (owner == mine) { running = false; owner = 0; renewed = 0; }
            if (authorized == mine) authorized = 0;
        }
        HomewardActivity.receptionChanged(mine, false);
        stopForeground(STOP_FOREGROUND_REMOVE);
        super.onDestroy();
    }
}
