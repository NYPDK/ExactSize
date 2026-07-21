package io.exactsize.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.IBinder;
import android.os.PowerManager;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashSet;
import java.util.Locale;
import java.util.Set;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicBoolean;

public final class EncodingService extends Service {
    private static final String ACTION_START = "io.exactsize.app.action.START_ENCODING";
    private static final String ACTION_CANCEL = "io.exactsize.app.action.CANCEL_ENCODING";
    private static final String PROGRESS_CHANNEL = "exactsize_encoding";
    private static final String RESULT_CHANNEL = "exactsize_results";
    private static final int PROGRESS_NOTIFICATION = 1201;
    private static final int RESULT_NOTIFICATION = 1202;

    private static volatile boolean active;

    private final AtomicBoolean polling = new AtomicBoolean(false);
    private ExecutorService worker;
    private NotificationManager notifications;
    private PowerManager.WakeLock wakeLock;

    static void start(Context context) {
        Intent intent = new Intent(context, EncodingService.class).setAction(ACTION_START);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            context.startForegroundService(intent);
        } else {
            context.startService(intent);
        }
    }

    static boolean isActive() {
        return active;
    }

    @Override
    public void onCreate() {
        super.onCreate();
        active = true;
        worker = Executors.newSingleThreadExecutor();
        notifications = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        createChannels();
        acquireWakeLock();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? ACTION_START : intent.getAction();
        if (ACTION_CANCEL.equals(action)) {
            worker.execute(this::cancelCurrentJob);
            return START_NOT_STICKY;
        }

        acquireWakeLock();
        notifications.cancel(RESULT_NOTIFICATION);
        startForegroundCompat(progressNotification("Preparing compression…", 0, true, 0));
        if (polling.compareAndSet(false, true)) {
            worker.execute(this::pollCurrentJob);
        }
        return START_NOT_STICKY;
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    @Override
    public void onDestroy() {
        active = false;
        releaseWakeLock();
        if (worker != null) {
            worker.shutdownNow();
        }
        super.onDestroy();
    }

    private void pollCurrentJob() {
        boolean sawActiveJob = false;
        int idlePolls = 0;
        int failedPolls = 0;
        try {
            while (!Thread.currentThread().isInterrupted()) {
                JobStatus job;
                try {
                    job = fetchJob();
                    failedPolls = 0;
                } catch (Exception error) {
                    failedPolls++;
                    if (failedPolls >= 12) {
                        finishWithNotification("Compression status unavailable", "Open ExactSize to check the video.", false);
                        return;
                    }
                    sleep(750);
                    continue;
                }

                if ("queued".equals(job.state) || "running".equals(job.state)) {
                    sawActiveJob = true;
                    idlePolls = 0;
                    notifications.notify(PROGRESS_NOTIFICATION, progressNotification(
                            progressText(job),
                            clampProgress(job.progress),
                            job.progress <= 0,
                            job.encodedBytes));
                } else if (sawActiveJob && isTerminal(job.state)) {
                    showTerminal(job);
                    return;
                } else if (!sawActiveJob) {
                    // The Web UI starts this service just before POSTing the job.
                    // Ignore an old terminal/idle snapshot during that short gap.
                    idlePolls++;
                    if (idlePolls >= 20) {
                        stopForegroundCompat();
                        stopSelf();
                        return;
                    }
                }
                sleep(750);
            }
        } finally {
            polling.set(false);
        }
    }

    private JobStatus fetchJob() throws Exception {
        HttpURLConnection connection = openJobConnection("GET");
        try {
            int response = connection.getResponseCode();
            if (response < 200 || response >= 300) {
                throw new IllegalStateException("job status HTTP " + response);
            }
            JSONObject object = new JSONObject(readAll(connection.getInputStream()));
            JobStatus result = new JobStatus();
            result.state = object.optString("state", "idle");
            result.phase = object.optString("phase", "Encoding");
            result.message = object.optString("message", "");
            result.error = object.optString("error", "");
            result.progress = object.optDouble("progress", 0);
            result.remainingSeconds = object.optDouble("remainingSeconds", 0);
            result.encodedBytes = object.optLong("encodedBytes", 0);
            return result;
        } finally {
            connection.disconnect();
        }
    }

    private void cancelCurrentJob() {
        try {
            HttpURLConnection connection = openJobConnection("DELETE");
            try {
                connection.getResponseCode();
            } finally {
                connection.disconnect();
            }
            notifications.notify(PROGRESS_NOTIFICATION,
                    progressNotification("Canceling and cleaning temporary files…", 0, true, 0));
        } catch (Exception ignored) {
            finishWithNotification("Could not cancel compression", "Open ExactSize to check the video.", false);
        }
    }

    private HttpURLConnection openJobConnection(String method) throws Exception {
        String backend = MainActivity.getBackendUrl();
        if (backend == null || backend.isEmpty()) {
            throw new IllegalStateException("backend is not ready");
        }
        URL root = new URL(backend);
        URL endpoint = new URL(root.getProtocol(), root.getHost(), root.getPort(), "/api/jobs/current");
        String token = Uri.parse(backend).getQueryParameter("token");
        HttpURLConnection connection = (HttpURLConnection) endpoint.openConnection();
        connection.setRequestMethod(method);
        connection.setConnectTimeout(2_000);
        connection.setReadTimeout(2_000);
        connection.setUseCaches(false);
        if (token != null) {
            connection.setRequestProperty("X-ExactSize-Token", token);
        }
        return connection;
    }

    private void showTerminal(JobStatus job) {
        if ("completed".equals(job.state)) {
            String detail = "Verified under target";
            if (job.encodedBytes > 0) {
                detail += " • " + formatBytes(job.encodedBytes);
            }
            finishWithNotification("Compression complete", detail, true);
            return;
        }
        if ("canceled".equals(job.state)) {
            finishWithNotification("Compression canceled", "Temporary files were cleaned up.", false);
            return;
        }
        String detail = job.error.isEmpty() ? job.message : summarizeError(job.error);
        if (detail.isEmpty()) {
            detail = "Open ExactSize for details.";
        }
        finishWithNotification("Compression failed", detail, false);
    }

    private void finishWithNotification(String title, String text, boolean success) {
        releaseWakeLock();
        stopForegroundCompat();
        notifications.notify(RESULT_NOTIFICATION, resultNotification(title, text, success));
        stopSelf();
    }

    private Notification progressNotification(String text, int progress, boolean indeterminate, long bytes) {
        Notification.Builder builder = builder(PROGRESS_CHANNEL)
                .setSmallIcon(R.drawable.exactsize_notification)
                .setContentTitle("Compressing video")
                .setContentText(text)
                .setContentIntent(openAppIntent())
                .setOngoing(true)
                .setOnlyAlertOnce(true)
                .setCategory(Notification.CATEGORY_PROGRESS)
                .setProgress(100, progress, indeterminate)
                .addAction(android.R.drawable.ic_menu_close_clear_cancel, "Cancel", cancelIntent());
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            builder.setForegroundServiceBehavior(Notification.FOREGROUND_SERVICE_IMMEDIATE);
        }
        if (bytes > 0) {
            builder.setSubText(formatBytes(bytes));
        }
        return builder.build();
    }

    private Notification resultNotification(String title, String text, boolean success) {
        return builder(RESULT_CHANNEL)
                .setSmallIcon(R.drawable.exactsize_notification)
                .setContentTitle(title)
                .setContentText(text)
                .setStyle(new Notification.BigTextStyle().bigText(text))
                .setContentIntent(openAppIntent())
                .setAutoCancel(true)
                .setCategory(success ? Notification.CATEGORY_STATUS : Notification.CATEGORY_ERROR)
                .build();
    }

    private Notification.Builder builder(String channel) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            return new Notification.Builder(this, channel);
        }
        return new Notification.Builder(this);
    }

    private PendingIntent openAppIntent() {
        Intent intent = new Intent(this, MainActivity.class)
                .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP | Intent.FLAG_ACTIVITY_SINGLE_TOP);
        return PendingIntent.getActivity(this, 0, intent,
                PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
    }

    private PendingIntent cancelIntent() {
        Intent intent = new Intent(this, EncodingService.class).setAction(ACTION_CANCEL);
        return PendingIntent.getService(this, 1, intent,
                PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
    }

    private void createChannels() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) {
            return;
        }
        NotificationChannel progress = new NotificationChannel(
                PROGRESS_CHANNEL, "Compression progress", NotificationManager.IMPORTANCE_LOW);
        progress.setDescription("Ongoing ExactSize video compression progress");
        progress.setShowBadge(false);
        progress.setLightColor(Color.rgb(228, 255, 63));
        notifications.createNotificationChannel(progress);

        NotificationChannel results = new NotificationChannel(
                RESULT_CHANNEL, "Compression results", NotificationManager.IMPORTANCE_DEFAULT);
        results.setDescription("Alerts when an ExactSize compression finishes or fails");
        results.enableVibration(true);
        results.setLightColor(Color.rgb(228, 255, 63));
        notifications.createNotificationChannel(results);
    }

    private void acquireWakeLock() {
        if (wakeLock == null) {
            PowerManager power = (PowerManager) getSystemService(POWER_SERVICE);
            wakeLock = power.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "ExactSize:background-encoding");
            wakeLock.setReferenceCounted(false);
        }
        if (!wakeLock.isHeld()) {
            wakeLock.acquire();
        }
    }

    private void releaseWakeLock() {
        if (wakeLock != null && wakeLock.isHeld()) {
            wakeLock.release();
        }
    }

    private void stopForegroundCompat() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
            stopForeground(STOP_FOREGROUND_REMOVE);
        } else {
            stopForeground(true);
        }
    }

    private void startForegroundCompat(Notification notification) {
        if (Build.VERSION.SDK_INT >= 35) {
            startForeground(PROGRESS_NOTIFICATION, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROCESSING);
        } else if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(PROGRESS_NOTIFICATION, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
        } else {
            startForeground(PROGRESS_NOTIFICATION, notification);
        }
    }

    private static boolean isTerminal(String state) {
        return "completed".equals(state) || "failed".equals(state) || "canceled".equals(state);
    }

    private static int clampProgress(double progress) {
        return Math.max(0, Math.min(100, (int) Math.round(progress)));
    }

    private static String progressText(JobStatus job) {
        String phase = job.phase.isEmpty() ? "Encoding" : job.phase;
        String result = phase + " • " + clampProgress(job.progress) + "%";
        if (job.remainingSeconds > 0) {
            result += " • " + formatDuration(job.remainingSeconds) + " left";
        }
        return result;
    }

    private static String formatDuration(double seconds) {
        long total = Math.max(0, Math.round(seconds));
        long minutes = total / 60;
        long remainder = total % 60;
        return String.format(Locale.US, "%d:%02d", minutes, remainder);
    }

    private static String formatBytes(long bytes) {
        if (bytes < 1_000) {
            return bytes + " B";
        }
        double value = bytes;
        String[] units = {"KB", "MB", "GB"};
        for (String unit : units) {
            value /= 1_000.0;
            if (value < 1_000 || "GB".equals(unit)) {
                return String.format(Locale.US, value >= 100 ? "%.0f %s" : value >= 10 ? "%.1f %s" : "%.2f %s", value, unit);
            }
        }
        return bytes + " B";
    }

    private static String summarizeError(String error) {
        Set<String> unique = new LinkedHashSet<>();
        String preferred = "";
        for (String rawLine : error.split("\\r?\\n")) {
            String line = rawLine.trim();
            while (line.startsWith("[")) {
                int end = line.indexOf(']');
                if (end < 0) {
                    break;
                }
                line = line.substring(end + 1).trim();
            }
            if (line.isEmpty()) {
                continue;
            }
            unique.add(line);
            String lower = line.toLowerCase(Locale.US);
            if (preferred.isEmpty() && (lower.contains("not support")
                    || lower.contains("doesn't support")
                    || lower.contains("not available")
                    || lower.contains("unavailable"))) {
                preferred = line;
            }
        }
        String detail = preferred;
        if (detail.isEmpty() && !unique.isEmpty()) {
            detail = unique.iterator().next();
        }
        if (detail.length() > 240) {
            detail = detail.substring(0, 237).trim() + "…";
        }
        return detail;
    }

    private static String readAll(InputStream input) throws Exception {
        StringBuilder result = new StringBuilder();
        try (BufferedReader reader = new BufferedReader(new InputStreamReader(input, StandardCharsets.UTF_8))) {
            String line;
            while ((line = reader.readLine()) != null) {
                result.append(line);
            }
        }
        return result.toString();
    }

    private static void sleep(long millis) {
        try {
            Thread.sleep(millis);
        } catch (InterruptedException error) {
            Thread.currentThread().interrupt();
        }
    }

    private static final class JobStatus {
        String state = "idle";
        String phase = "";
        String message = "";
        String error = "";
        double progress;
        double remainingSeconds;
        long encodedBytes;
    }
}
