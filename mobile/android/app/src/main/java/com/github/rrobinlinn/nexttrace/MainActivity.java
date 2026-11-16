package com.github.rrobinlinn.nexttrace;

import android.app.Activity;
import android.content.ActivityNotFoundException;
import android.content.Intent;
import android.graphics.Color;
import android.graphics.Typeface;
import android.net.Uri;
import android.os.Bundle;
import android.util.Log;
import android.view.View;
import android.view.ViewGroup;
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
    private static final int START_TIMEOUT_MS = 20_000;
    private static final int MAX_LOG_LINES = 200;

    // The helper outlives Activity recreation, so it is process-wide state.
    private static final Object LOCK = new Object();
    private static final ArrayDeque<String> LOG_LINES = new ArrayDeque<>();
    private static volatile Process server;
    private static int serverPort;

    private WebView webView;
    private ScrollView statusScroll;
    private TextView statusView;
    private volatile boolean starting;

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
        statusView.setTextColor(Color.LTGRAY);
        statusView.setTextIsSelectable(false);
        int pad = (int) (16 * getResources().getDisplayMetrics().density);
        statusView.setPadding(pad, pad, pad, pad);
        statusView.setOnClickListener(v -> startServer());

        statusScroll = new ScrollView(this);
        statusScroll.setBackgroundColor(Color.rgb(15, 23, 42));
        statusScroll.setFillViewport(true);
        statusScroll.addView(statusView, new ScrollView.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        root.addView(statusScroll, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        setContentView(root);
        startServer();
    }

    private void configureWebView(WebView view) {
        WebSettings settings = view.getSettings();
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(true);
        settings.setBuiltInZoomControls(true);
        settings.setDisplayZoomControls(false);
        view.setWebChromeClient(new WebChromeClient());
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
                String url = "http://" + LOOPBACK + ":" + port + "/";
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

    private static int pickFreePort() throws IOException {
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

    @Override
    public void onBackPressed() {
        if (webView != null && webView.getVisibility() == View.VISIBLE && webView.canGoBack()) {
            webView.goBack();
            return;
        }
        super.onBackPressed();
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
