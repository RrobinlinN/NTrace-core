package com.github.rrobinlinn.nexttrace;

import android.app.Activity;
import android.content.ActivityNotFoundException;
import android.content.Intent;
import android.content.res.Configuration;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.ColorDrawable;
import android.net.Uri;
import android.os.Bundle;
import android.util.Log;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.JavascriptInterface;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceError;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;
import android.widget.ScrollView;
import android.widget.TextView;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStreamReader;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.ArrayDeque;
import java.util.Map;

/**
 * Thin shell around the NextTrace executable.
 *
 * The executable ships as lib/arm64-v8a/libnexttrace.so so Android extracts it
 * to nativeLibraryDir, the only app location code may be executed from. It is
 * started in deploy mode on a loopback port and its Web UI is shown in a
 * WebView. Probes therefore originate from this device.
 */
public class MainActivity extends Activity {
    private static final String TAG = "NextTrace";
    private static final String BINARY_NAME = "libnexttrace.so";
    private static final String LOOPBACK = "127.0.0.1";
    private static final int PREFERRED_PORT = 31780;
    private static final int START_TIMEOUT_MS = 20_000;
    private static final int MAX_LOG_LINES = 200;
    // Match --shell-bg (dark and light) in server/web/assets/app_shell.css.
    private static final int DARK_BACKGROUND = Color.rgb(11, 18, 32);
    private static final int LIGHT_BACKGROUND = Color.rgb(242, 244, 248);
    private static final String PREFS = "shell";
    // Last theme the page reported, so the next launch starts in it:
    // "light", "dark", or "system" when the page follows the system.
    private static final String PREF_THEME = "theme";

    // The helper outlives Activity recreation, so it is process-wide state.
    private static final Object LOCK = new Object();
    private static final ArrayDeque<String> LOG_LINES = new ArrayDeque<>();
    private static volatile Process server;
    private static int serverPort;

    private WebView webView;
    private ScrollView statusScroll;
    private TextView statusView;
    private volatile boolean starting;
    private boolean darkTheme;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);

        FrameLayout root = new FrameLayout(this);
        root.setFitsSystemWindows(true);

        webView = new WebView(this);
        webView.setVisibility(View.INVISIBLE);
        configureWebView(webView);
        root.addView(webView, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        statusView = new TextView(this);
        statusView.setTypeface(Typeface.MONOSPACE);
        statusView.setTextSize(12);
        statusView.setTextIsSelectable(false);
        int pad = (int) (16 * getResources().getDisplayMetrics().density);
        statusView.setPadding(pad, pad, pad, pad);
        statusView.setOnClickListener(v -> startServer());

        statusScroll = new ScrollView(this);
        statusScroll.setFillViewport(true);
        statusScroll.addView(statusView, new ScrollView.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        root.addView(statusScroll, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        setContentView(root);
        applyTheme(savedThemeDark());
        startServer();
    }

    private void configureWebView(WebView view) {
        WebSettings settings = view.getSettings();
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(true);
        // Behave like an app surface, not a browser page: no pinch zoom, no
        // overscroll glow, no scrollbars.
        settings.setSupportZoom(false);
        settings.setBuiltInZoomControls(false);
        settings.setDisplayZoomControls(false);
        view.setOverScrollMode(View.OVER_SCROLL_NEVER);
        view.setVerticalScrollBarEnabled(false);
        view.setHorizontalScrollBarEnabled(false);
        view.setWebChromeClient(new WebChromeClient());
        // Only the loopback console is ever shown here (other URLs open in the
        // browser); it reports its resolved light/dark theme through this.
        view.addJavascriptInterface(new ThemeBridge(), "NextTraceShell");
        view.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView v, WebResourceRequest request) {
                Uri uri = request.getUrl();
                if (LOOPBACK.equals(uri.getHost())) {
                    return false;
                }
                // Anything that leaves the local console opens in the browser.
                try {
                    startActivity(new Intent(Intent.ACTION_VIEW, uri));
                } catch (ActivityNotFoundException e) {
                    Log.w(TAG, "no activity for " + uri, e);
                }
                return true;
            }

            @Override
            public void onReceivedError(WebView v, WebResourceRequest request, WebResourceError error) {
                if (request.isForMainFrame()) {
                    showFailure(getString(R.string.status_failed) + "\n\n"
                            + error.getDescription() + "\n\n" + recentLog());
                }
            }
        });
    }

    /** Starts the helper if needed, then loads its Web UI. */
    private void startServer() {
        if (starting) {
            return;
        }
        starting = true;
        showStatus(getString(R.string.status_starting));
        new Thread(() -> {
            try {
                int port = ensureServer();
                // shell=app switches the console to its app layout (app_shell.js).
                // system= tells it the system appearance, which this WebView
                // does not expose through prefers-color-scheme.
                String url = "http://" + LOOPBACK + ":" + port + "/?shell=app&system="
                        + (systemDark() ? "dark" : "light");
                runOnUiThread(() -> {
                    statusScroll.setVisibility(View.GONE);
                    webView.setVisibility(View.VISIBLE);
                    webView.loadUrl(url);
                });
            } catch (Exception e) {
                Log.e(TAG, "start failed", e);
                showFailure(getString(R.string.status_failed) + "\n\n" + e.getMessage()
                        + "\n\n" + recentLog());
            } finally {
                starting = false;
            }
        }, "nexttrace-start").start();
    }

    private int ensureServer() throws IOException, InterruptedException {
        synchronized (LOCK) {
            if (server != null && server.isAlive()) {
                return serverPort;
            }

            File binary = new File(getApplicationInfo().nativeLibraryDir, BINARY_NAME);
            if (!binary.isFile()) {
                throw new IOException(getString(R.string.status_missing_binary) + "\n" + binary);
            }

            int port = pickFreePort();
            File home = getFilesDir();
            ProcessBuilder builder = new ProcessBuilder(
                    binary.getAbsolutePath(), "--deploy", "--listen", LOOPBACK + ":" + port);
            builder.directory(home);
            builder.redirectErrorStream(true);
            Map<String, String> env = builder.environment();
            env.put("HOME", home.getAbsolutePath());
            env.put("XDG_CONFIG_HOME", new File(home, ".config").getAbsolutePath());
            env.put("TMPDIR", getCacheDir().getAbsolutePath());
            // The helper exits when this process (and so its stdin pipe) goes away.
            env.put("NEXTTRACE_EXIT_ON_STDIN_CLOSE", "1");

            synchronized (LOG_LINES) {
                LOG_LINES.clear();
            }
            Process process = builder.start();
            pumpLog(process);
            server = process;
            serverPort = port;

            long deadline = System.currentTimeMillis() + START_TIMEOUT_MS;
            while (System.currentTimeMillis() < deadline) {
                if (!process.isAlive()) {
                    server = null;
                    throw new IOException("exit code " + process.exitValue());
                }
                if (canConnect(port)) {
                    return port;
                }
                Thread.sleep(100);
            }
            process.destroy();
            server = null;
            throw new IOException("timed out waiting for " + LOOPBACK + ":" + port);
        }
    }

    // The page keeps its settings and recent targets in localStorage, which is
    // scoped to the origin including the port. Prefer one fixed port so they
    // survive restarts, and only fall back to a random one if it is taken.
    private static int pickFreePort() throws IOException {
        try (ServerSocket socket = new ServerSocket(PREFERRED_PORT, 1, InetAddress.getByName(LOOPBACK))) {
            return socket.getLocalPort();
        } catch (IOException e) {
            Log.w(TAG, "port " + PREFERRED_PORT + " is busy, using a random one", e);
        }
        try (ServerSocket socket = new ServerSocket(0, 1, InetAddress.getByName(LOOPBACK))) {
            return socket.getLocalPort();
        }
    }

    private static boolean canConnect(int port) {
        try (Socket socket = new Socket()) {
            socket.connect(new InetSocketAddress(LOOPBACK, port), 200);
            return true;
        } catch (IOException e) {
            return false;
        }
    }

    private static void pumpLog(Process process) {
        Thread thread = new Thread(() -> {
            try (BufferedReader reader = new BufferedReader(
                    new InputStreamReader(process.getInputStream(), StandardCharsets.UTF_8))) {
                String line;
                while ((line = reader.readLine()) != null) {
                    Log.i(TAG, line);
                    synchronized (LOG_LINES) {
                        if (LOG_LINES.size() >= MAX_LOG_LINES) {
                            LOG_LINES.removeFirst();
                        }
                        LOG_LINES.addLast(line);
                    }
                }
            } catch (IOException e) {
                Log.w(TAG, "log pump stopped", e);
            }
        }, "nexttrace-log");
        thread.setDaemon(true);
        thread.start();
    }

    private static String recentLog() {
        StringBuilder out = new StringBuilder();
        synchronized (LOG_LINES) {
            for (String line : LOG_LINES) {
                out.append(line).append('\n');
            }
        }
        return out.toString();
    }

    private void showStatus(String text) {
        runOnUiThread(() -> {
            statusView.setText(text);
            statusScroll.setVisibility(View.VISIBLE);
        });
    }

    private void showFailure(String text) {
        runOnUiThread(() -> {
            webView.setVisibility(View.INVISIBLE);
            statusView.setText(text);
            statusScroll.setVisibility(View.VISIBLE);
        });
    }

    /** The page's last theme, until the page itself reports one. */
    private boolean savedThemeDark() {
        String saved = getSharedPreferences(PREFS, MODE_PRIVATE).getString(PREF_THEME, "system");
        if ("light".equals(saved) || "dark".equals(saved)) {
            return "dark".equals(saved);
        }
        return systemDark();
    }

    private boolean systemDark() {
        int night = getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK;
        return night == Configuration.UI_MODE_NIGHT_YES;
    }

    /**
     * One colour for window, system bars and page, so nothing flashes and the
     * bars read as part of the app; bar icons are dark on the light theme.
     */
    @SuppressWarnings("deprecation")
    private void applyTheme(boolean dark) {
        darkTheme = dark;
        int background = dark ? DARK_BACKGROUND : LIGHT_BACKGROUND;
        getWindow().setBackgroundDrawable(new ColorDrawable(background));
        getWindow().setStatusBarColor(background);
        getWindow().setNavigationBarColor(background);
        View decor = getWindow().getDecorView();
        int flags = decor.getSystemUiVisibility()
                & ~(View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR);
        if (!dark) {
            flags |= View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR;
        }
        decor.setSystemUiVisibility(flags);
        if (webView != null) {
            webView.setBackgroundColor(background);
            ((View) webView.getParent()).setBackgroundColor(background);
        }
        if (statusScroll != null) {
            statusScroll.setBackgroundColor(background);
            statusView.setTextColor(dark ? Color.LTGRAY : Color.DKGRAY);
        }
    }

    @Override
    public void onConfigurationChanged(Configuration newConfig) {
        super.onConfigurationChanged(newConfig);
        // uiMode is in configChanges, so a light/dark switch lands here instead
        // of recreating the activity. The page decides whether it follows.
        String system = systemDark() ? "dark" : "light";
        if (webView != null && webView.getVisibility() == View.VISIBLE) {
            webView.evaluateJavascript("window.nextTraceShellSystemTheme && window.nextTraceShellSystemTheme('"
                    + system + "')", null);
        } else {
            applyTheme(savedThemeDark());
        }
    }

    /** Called by app_shell.js with the theme the page resolved. */
    final class ThemeBridge {
        @JavascriptInterface
        public void setTheme(String theme, boolean followsSystem) {
            if (!"dark".equals(theme) && !"light".equals(theme)) {
                return;
            }
            boolean dark = "dark".equals(theme);
            String saved = followsSystem ? "system" : theme;
            runOnUiThread(() -> {
                if (isDestroyed()) {
                    return;
                }
                if (dark != darkTheme) {
                    applyTheme(dark);
                }
                getSharedPreferences(PREFS, MODE_PRIVATE).edit().putString(PREF_THEME, saved).apply();
            });
        }
    }

    @Override
    public void onBackPressed() {
        if (webView == null || webView.getVisibility() != View.VISIBLE) {
            super.onBackPressed();
            return;
        }
        // Let the page consume Back first (it closes the options sheet).
        webView.evaluateJavascript(
                "(window.nextTraceShellBack && window.nextTraceShellBack()) === true",
                handled -> {
                    if (!"true".equals(handled)) {
                        MainActivity.super.onBackPressed();
                    }
                });
    }

    @Override
    protected void onDestroy() {
        if (isFinishing()) {
            // Not under LOCK: a start still in progress may hold it for a while.
            // ensureServer() notices the dead process on the next launch.
            Process process = server;
            if (process != null) {
                process.destroy();
            }
        }
        if (webView != null) {
            webView.destroy();
            webView = null;
        }
        super.onDestroy();
    }
}
