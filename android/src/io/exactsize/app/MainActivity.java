package io.exactsize.app;

import android.Manifest;
import android.app.Activity;
import android.content.ClipData;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.net.Uri;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.Build;
import android.webkit.JavascriptInterface;
import android.webkit.MimeTypeMap;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;
import android.widget.TextView;
import android.widget.Toast;
import android.view.View;
import android.view.Window;
import android.view.WindowManager;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.lang.ref.WeakReference;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.Map;

public final class MainActivity extends Activity {
    private static final int REQUEST_OPEN_VIDEO = 4101;
    private static final int REQUEST_SAVE_OUTPUT = 4102;
    private static final int REQUEST_NOTIFICATIONS = 4103;
    private static final Object BACKEND_LOCK = new Object();
    private static volatile Process backendProcess;
    private static volatile Thread backendThread;
    private static volatile String backendUrl;
    private static volatile boolean shuttingDown;
    private static WeakReference<MainActivity> activeActivity = new WeakReference<>(null);

    private final Handler mainHandler = new Handler(Looper.getMainLooper());
    private WebView webView;
    private TextView statusView;
    private ValueCallback<Uri[]> pendingFileChooser;
    private File pendingOutput;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);

        synchronized (BACKEND_LOCK) {
            activeActivity = new WeakReference<>(this);
        }

        configureWindow();

        FrameLayout root = new FrameLayout(this);
        root.setBackgroundColor(Color.rgb(24, 24, 27));
        root.setOnApplyWindowInsetsListener((view, insets) -> {
            int left = insets.getSystemWindowInsetLeft();
            int top = insets.getSystemWindowInsetTop();
            int right = insets.getSystemWindowInsetRight();
            int bottom = insets.getSystemWindowInsetBottom();
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P && insets.getDisplayCutout() != null) {
                left = Math.max(left, insets.getDisplayCutout().getSafeInsetLeft());
                top = Math.max(top, insets.getDisplayCutout().getSafeInsetTop());
                right = Math.max(right, insets.getDisplayCutout().getSafeInsetRight());
                bottom = Math.max(bottom, insets.getDisplayCutout().getSafeInsetBottom());
            }
            view.setPadding(left, top, right, bottom);
            return insets;
        });

        webView = new WebView(this);
        webView.setVisibility(WebView.INVISIBLE);
        root.addView(webView, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT,
                FrameLayout.LayoutParams.MATCH_PARENT));

        statusView = new TextView(this);
        statusView.setText("Starting ExactSize…");
        statusView.setTextColor(Color.WHITE);
        statusView.setTextSize(17);
        statusView.setGravity(android.view.Gravity.CENTER);
        statusView.setPadding(48, 48, 48, 48);
        root.addView(statusView, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT,
                FrameLayout.LayoutParams.MATCH_PARENT));

        setContentView(root);
        root.requestApplyInsets();
        configureWebView();
        startBackend();
    }

    private void configureWindow() {
        Window window = getWindow();
        window.setStatusBarColor(Color.rgb(27, 28, 25));
        window.setNavigationBarColor(Color.rgb(21, 22, 19));
        window.getDecorView().setSystemUiVisibility(0);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            window.setStatusBarContrastEnforced(false);
            window.setNavigationBarContrastEnforced(false);
        }
    }

    private void configureWebView() {
        WebSettings settings = webView.getSettings();
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(true);
        settings.setAllowFileAccess(false);
        settings.setAllowContentAccess(true);
        settings.setMediaPlaybackRequiresUserGesture(true);
        settings.setBuiltInZoomControls(false);
        settings.setDisplayZoomControls(false);

        webView.addJavascriptInterface(new AndroidBridge(), "ExactSizeAndroid");
        webView.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                Uri uri = request.getUrl();
                String host = uri.getHost();
                if (("http".equals(uri.getScheme()) || "https".equals(uri.getScheme()))
                        && ("127.0.0.1".equals(host) || "localhost".equals(host))) {
                    return false;
                }
                try {
                    startActivity(new Intent(Intent.ACTION_VIEW, uri));
                } catch (RuntimeException error) {
                    Toast.makeText(MainActivity.this, "No app can open that link", Toast.LENGTH_SHORT).show();
                }
                return true;
            }

            @Override
            public void onPageFinished(WebView view, String url) {
                webView.setVisibility(WebView.VISIBLE);
                statusView.setVisibility(TextView.GONE);
            }
        });
        webView.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(
                    WebView view,
                    ValueCallback<Uri[]> callback,
                    FileChooserParams params) {
                if (pendingFileChooser != null) {
                    pendingFileChooser.onReceiveValue(null);
                }
                pendingFileChooser = callback;

                Intent intent = new Intent(Intent.ACTION_OPEN_DOCUMENT);
                intent.addCategory(Intent.CATEGORY_OPENABLE);
                intent.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
                intent.setType(bestMimeType(params.getAcceptTypes()));
                intent.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, params.getMode() == FileChooserParams.MODE_OPEN_MULTIPLE);
                try {
                    startActivityForResult(intent, REQUEST_OPEN_VIDEO);
                    return true;
                } catch (RuntimeException error) {
                    pendingFileChooser = null;
                    callback.onReceiveValue(null);
                    Toast.makeText(MainActivity.this, "No document picker is available", Toast.LENGTH_LONG).show();
                    return false;
                }
            }
        });
    }

    private static String bestMimeType(String[] acceptTypes) {
        if (acceptTypes != null) {
            for (String accept : acceptTypes) {
                if (accept == null) {
                    continue;
                }
                for (String candidate : accept.split(",")) {
                    candidate = candidate.trim();
                    if (candidate.contains("/") && !"*/*".equals(candidate)) {
                        return candidate;
                    }
                }
            }
        }
        return "video/*";
    }

    private void startBackend() {
        synchronized (BACKEND_LOCK) {
            if (backendProcess != null && backendProcess.isAlive()) {
                if (backendUrl != null) {
                    loadBackend(backendUrl);
                }
                return;
            }
            if (backendThread != null && backendThread.isAlive()) {
                return;
            }
            shuttingDown = false;
            backendThread = new Thread(() -> {
                File nativeDir = new File(getApplicationInfo().nativeLibraryDir);
                File backend = new File(nativeDir, "libexactsize.so");
                File ffmpeg = new File(nativeDir, "libffmpeg.so");
                File ffprobe = new File(nativeDir, "libffprobe.so");

                if (!backend.canExecute() || !ffmpeg.canExecute() || !ffprobe.canExecute()) {
                    showFatalOnActive("The bundled Android executables were not extracted correctly.");
                    return;
                }

                ProcessBuilder builder = new ProcessBuilder(backend.getAbsolutePath());
                builder.redirectErrorStream(true);
                builder.directory(getFilesDir());
                Map<String, String> environment = builder.environment();
                environment.put("EXACTSIZE_HEADLESS", "1");
                environment.put("EXACTSIZE_FFMPEG", ffmpeg.getAbsolutePath());
                environment.put("EXACTSIZE_FFPROBE", ffprobe.getAbsolutePath());
                environment.put("HOME", getFilesDir().getAbsolutePath());
                environment.put("TMPDIR", getCacheDir().getAbsolutePath());
                environment.put("XDG_CACHE_HOME", new File(getCacheDir(), "xdg").getAbsolutePath());
                environment.put("PATH", nativeDir.getAbsolutePath() + ":/system/bin:/system/xbin");

                StringBuilder earlyOutput = new StringBuilder();
                boolean loaded = false;
                try {
                    backendProcess = builder.start();
                    try (BufferedReader reader = new BufferedReader(
                            new InputStreamReader(backendProcess.getInputStream()))) {
                        String line;
                        while ((line = reader.readLine()) != null) {
                            if (!loaded && isLocalBackendUrl(line)) {
                                loaded = true;
                                final String url = line.trim();
                                backendUrl = url;
                                loadBackendOnActive(url);
                            } else if (!loaded && earlyOutput.length() < 4096) {
                                earlyOutput.append(line).append('\n');
                            }
                        }
                    }
                    int exitCode = backendProcess.waitFor();
                    if (!shuttingDown) {
                        String detail = earlyOutput.toString().trim();
                        showFatalOnActive("ExactSize backend stopped (exit " + exitCode + ")."
                                + (detail.isEmpty() ? "" : "\n\n" + detail));
                    }
                } catch (Exception error) {
                    if (!shuttingDown) {
                        showFatalOnActive("Could not start ExactSize: " + error.getMessage());
                    }
                } finally {
                    synchronized (BACKEND_LOCK) {
                        backendProcess = null;
                        backendThread = null;
                        backendUrl = null;
                    }
                }
            }, "exactsize-backend");
            backendThread.start();
        }
    }

    private static void loadBackendOnActive(String url) {
        MainActivity activity;
        synchronized (BACKEND_LOCK) {
            activity = activeActivity.get();
        }
        if (activity != null) {
            activity.loadBackend(url);
        }
    }

    private void loadBackend(String url) {
        mainHandler.post(() -> {
            if (webView != null) {
                webView.loadUrl(url);
            }
        });
    }

    static String getBackendUrl() {
        return backendUrl;
    }

    static void shutdownBackendIfNoActivity() {
        synchronized (BACKEND_LOCK) {
            if (activeActivity.get() == null) {
                shutdownBackendLocked();
            }
        }
    }

    private static void shutdownBackend() {
        synchronized (BACKEND_LOCK) {
            shutdownBackendLocked();
        }
    }

    private static void shutdownBackendLocked() {
        shuttingDown = true;
        if (backendProcess != null) {
            backendProcess.destroy();
        }
    }

    private static boolean isLocalBackendUrl(String line) {
        String value = line.trim().toLowerCase(Locale.US);
        return value.startsWith("http://127.0.0.1:") || value.startsWith("http://localhost:");
    }

    private void showFatal(String message) {
        mainHandler.post(() -> {
            webView.setVisibility(WebView.INVISIBLE);
            statusView.setText(message);
            statusView.setVisibility(TextView.VISIBLE);
        });
    }

    private static void showFatalOnActive(String message) {
        MainActivity activity;
        synchronized (BACKEND_LOCK) {
            activity = activeActivity.get();
        }
        if (activity != null) {
            activity.showFatal(message);
        }
    }

    private final class AndroidBridge {
        @JavascriptInterface
        public void saveOutput(String path) {
            mainHandler.post(() -> beginSaveOutput(path));
        }

        @JavascriptInterface
        public void setEncoding(boolean encoding) {
            mainHandler.post(() -> setEncodingPowerState(encoding));
        }
    }

    private void setEncodingPowerState(boolean encoding) {
        if (encoding) {
            getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
            requestNotificationPermission();
            EncodingService.start(this);
            return;
        }
        getWindow().clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
    }

    private void requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQUEST_NOTIFICATIONS);
        }
    }

    private void beginSaveOutput(String path) {
        try {
            File candidate = new File(path).getCanonicalFile();
            if (!candidate.isFile() || (!isInside(candidate, getFilesDir()) && !isInside(candidate, getCacheDir()))) {
                Toast.makeText(this, "ExactSize cannot export that file", Toast.LENGTH_LONG).show();
                return;
            }
            pendingOutput = candidate;
            Intent intent = new Intent(Intent.ACTION_CREATE_DOCUMENT);
            intent.addCategory(Intent.CATEGORY_OPENABLE);
            intent.addFlags(Intent.FLAG_GRANT_WRITE_URI_PERMISSION);
            intent.setType(mimeTypeFor(candidate.getName()));
            intent.putExtra(Intent.EXTRA_TITLE, candidate.getName());
            startActivityForResult(intent, REQUEST_SAVE_OUTPUT);
        } catch (IOException | RuntimeException error) {
            pendingOutput = null;
            Toast.makeText(this, "Could not prepare the output export", Toast.LENGTH_LONG).show();
        }
    }

    private static boolean isInside(File file, File directory) throws IOException {
        String child = file.getCanonicalPath();
        String parent = directory.getCanonicalPath();
        return child.equals(parent) || child.startsWith(parent + File.separator);
    }

    private static String mimeTypeFor(String name) {
        String extension = MimeTypeMap.getFileExtensionFromUrl(name);
        String result = MimeTypeMap.getSingleton().getMimeTypeFromExtension(extension.toLowerCase(Locale.US));
        return result == null ? "video/*" : result;
    }

    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode == REQUEST_OPEN_VIDEO) {
            ValueCallback<Uri[]> callback = pendingFileChooser;
            pendingFileChooser = null;
            if (callback == null) {
                return;
            }
            callback.onReceiveValue(resultCode == RESULT_OK ? selectedUris(data) : null);
            return;
        }
        if (requestCode == REQUEST_SAVE_OUTPUT) {
            File source = pendingOutput;
            pendingOutput = null;
            Uri destination = resultCode == RESULT_OK && data != null ? data.getData() : null;
            if (source != null && destination != null) {
                new Thread(() -> copyOutput(source, destination), "exactsize-export").start();
            }
        }
    }

    private static Uri[] selectedUris(Intent data) {
        if (data == null) {
            return null;
        }
        ClipData clip = data.getClipData();
        if (clip == null) {
            return data.getData() == null ? null : new Uri[]{data.getData()};
        }
        List<Uri> uris = new ArrayList<>();
        for (int index = 0; index < clip.getItemCount(); index++) {
            Uri uri = clip.getItemAt(index).getUri();
            if (uri != null) {
                uris.add(uri);
            }
        }
        return uris.isEmpty() ? null : uris.toArray(new Uri[0]);
    }

    private void copyOutput(File source, Uri destination) {
        try (InputStream input = new FileInputStream(source);
             OutputStream output = getContentResolver().openOutputStream(destination, "w")) {
            if (output == null) {
                throw new IOException("the selected destination cannot be opened");
            }
            byte[] buffer = new byte[1024 * 1024];
            int read;
            while ((read = input.read(buffer)) != -1) {
                output.write(buffer, 0, read);
            }
            output.flush();
            mainHandler.post(() -> Toast.makeText(
                    MainActivity.this, "Output saved", Toast.LENGTH_LONG).show());
        } catch (IOException error) {
            mainHandler.post(() -> Toast.makeText(
                    MainActivity.this, "Could not save output: " + error.getMessage(), Toast.LENGTH_LONG).show());
        }
    }

    @Override
    public void onBackPressed() {
        moveTaskToBack(true);
    }

    @Override
    protected void onDestroy() {
        getWindow().clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        if (pendingFileChooser != null) {
            pendingFileChooser.onReceiveValue(null);
            pendingFileChooser = null;
        }
        synchronized (BACKEND_LOCK) {
            if (activeActivity.get() == this) {
                activeActivity.clear();
            }
        }
        if (!EncodingService.isActive()) {
            shutdownBackend();
        }
        if (webView != null) {
            webView.stopLoading();
            webView.loadUrl("about:blank");
            webView.removeJavascriptInterface("ExactSizeAndroid");
            webView.destroy();
        }
        super.onDestroy();
    }
}
