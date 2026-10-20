// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package bot.h6e.yggchat;

import android.app.Activity;
import android.content.Intent;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.net.Uri;
import android.webkit.*;

public class MainActivity extends Activity {
    static final String HOME = "http://127.0.0.1:7780/";
    private WebView web;
    // True only while the main frame shows the app's own UI; the YggAndroid
    // bridge refuses to act for any other page.
    private volatile boolean onHome;
    // Pending <input type=file> callback from the WebView.
    private ValueCallback<Uri[]> fileCb;
    static final int FILE_REQ = 3;
    // Camera request from the page (QR scanner) waiting on the runtime permission.
    private PermissionRequest camReq;
    static final int CAM_REQ = 4;

    // Opens the UI through the launch URL, which gives the WebView the API key
    // as an HttpOnly cookie. Waits for the node to create the key on first run.
    void loadHome(final WebView v, final int tries) {
        String k = YggService.uiKey(this);
        if (k.isEmpty() && tries < 50) {
            new Handler().postDelayed(new Runnable(){ public void run(){ loadHome(v, tries + 1); }}, 200);
            return;
        }
        v.loadUrl(HOME + "?k=" + k);
    }

    static boolean isHome(String u) { return u != null && u.startsWith(HOME); }

    // Anything that is not the app's own UI goes to the user's browser.
    void openOutside(Uri u) {
        String sc = u.getScheme();
        if (!"https".equalsIgnoreCase(sc) && !"http".equalsIgnoreCase(sc)) return;
        try { startActivity(new Intent(Intent.ACTION_VIEW, u).addCategory(Intent.CATEGORY_BROWSABLE)); } catch (Exception e) {}
    }
    static volatile boolean visible;

    @Override protected void onResume() { super.onResume(); visible = true; }
    @Override protected void onPause() { visible = false; super.onPause(); }

    String notifState() {
        android.app.NotificationManager nm = (android.app.NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission("android.permission.POST_NOTIFICATIONS") != android.content.pm.PackageManager.PERMISSION_GRANTED)
            return getSharedPreferences("ygg", 0).getBoolean("asked", false) && !shouldShowRequestPermissionRationale("android.permission.POST_NOTIFICATIONS") ? "denied" : "default";
        return (Build.VERSION.SDK_INT < 24 || nm.areNotificationsEnabled()) ? "granted" : "denied";
    }

    @Override protected void onCreate(Bundle s) {
        super.onCreate(s);
        Intent svc = new Intent(this, YggService.class);
        if (Build.VERSION.SDK_INT >= 26) startForegroundService(svc); else startService(svc);
        if (getSharedPreferences("ygg", 0).getBoolean("vpn", false) && android.net.VpnService.prepare(this) == null)
            startService(new Intent(this, YggVpn.class));
        web = new WebView(this);
        web.setBackgroundColor(0xff0b0f0b);
        WebSettings ws = web.getSettings();
        ws.setJavaScriptEnabled(true);
        ws.setDomStorageEnabled(true);
        ws.setAllowFileAccess(false);
        ws.setAllowContentAccess(false);
        ws.setSupportMultipleWindows(false);
        ws.setJavaScriptCanOpenWindowsAutomatically(false);
        ws.setGeolocationEnabled(false);
        web.addJavascriptInterface(new Object() {
            // Screen security (FLAG_SECURE): blocks screenshots and hides the app in recents.
            @JavascriptInterface public void setSecure(final boolean on) {
                if (!onHome) return;
                runOnUiThread(new Runnable() { public void run() {
                    if (on) getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_SECURE);
                    else getWindow().clearFlags(android.view.WindowManager.LayoutParams.FLAG_SECURE);
                }});
            }
            // Notifications: WebView has no Web Notification API, so the service posts them natively.
            @JavascriptInterface public String notifState() { return onHome ? MainActivity.this.notifState() : "denied"; }
            @JavascriptInterface public void requestNotif() {
                if (!onHome) return;
                runOnUiThread(new Runnable() { public void run() {
                    String st = notifState();
                    if ("default".equals(st) && Build.VERSION.SDK_INT >= 33) {
                        getSharedPreferences("ygg", 0).edit().putBoolean("asked", true).apply();
                        requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 1);
                    } else if (!"granted".equals(st)) {
                        Intent i = new Intent("android.settings.APP_NOTIFICATION_SETTINGS");
                        i.putExtra("android.provider.extra.APP_PACKAGE", getPackageName());
                        try { startActivity(i); } catch (Exception e) {}
                    }
                }});
            }
            // VPN mode: YggChat becomes the device's Yggdrasil for every app.
            @JavascriptInterface public boolean vpnSupported() { return onHome; }
            @JavascriptInterface public void setVpn(final boolean on) {
                if (!onHome) return;
                runOnUiThread(new Runnable() { public void run() {
                    getSharedPreferences("ygg", 0).edit().putBoolean("vpn", on).apply();
                    if (!on) { startService(new Intent(MainActivity.this, YggVpn.class).setAction(YggVpn.STOP)); vpnResult(true); return; }
                    Intent prep = android.net.VpnService.prepare(MainActivity.this);
                    if (prep != null) { try { startActivityForResult(prep, 2); } catch (Exception e) { vpnResult(false); } }
                    else { startService(new Intent(MainActivity.this, YggVpn.class)); vpnResult(true); }
                }});
            }
            @JavascriptInterface public void setNotif(boolean on, boolean hide) {
                if (!onHome) return;
                getSharedPreferences("ygg", 0).edit().putBoolean("notif", on).putBoolean("hide", hide).apply();
            }
        }, "YggAndroid");
        web.setWebViewClient(new WebViewClient() {
            // Only the local UI is ever shown in this WebView; links, downloads
            // and redirects elsewhere open in the browser.
            @Override public boolean shouldOverrideUrlLoading(WebView v, WebResourceRequest r) {
                Uri u = r.getUrl();
                if (isHome(u.toString())) return false;
                if (r.isForMainFrame()) openOutside(u);
                return true;
            }
            @SuppressWarnings("deprecation")
            @Override public boolean shouldOverrideUrlLoading(WebView v, String url) {
                if (isHome(url)) return false;
                openOutside(Uri.parse(url));
                return true;
            }
            @Override public void onPageStarted(WebView v, String url, android.graphics.Bitmap f) {
                onHome = isHome(url);
                if (!onHome) { v.stopLoading(); openOutside(Uri.parse(url)); loadHome(v, 0); }
            }
            @Override public void doUpdateVisitedHistory(WebView v, String url, boolean reload) {
                onHome = isHome(url);
            }
            @Override public void onReceivedError(WebView v, WebResourceRequest r, WebResourceError e) {
                if (r.isForMainFrame()) { final WebView vv = v; new Handler().postDelayed(new Runnable(){ public void run(){ loadHome(vv, 0); }}, 700); }
            }
        });
        // <input type="file">: WebView shows nothing unless the app opens a picker.
        web.setWebChromeClient(new WebChromeClient() {
            // getUserMedia (QR scanner): camera only, only for the app's own UI.
            @Override public void onPermissionRequest(final PermissionRequest r) {
                runOnUiThread(new Runnable() { public void run() {
                    boolean cam = false;
                    for (String res : r.getResources()) if (PermissionRequest.RESOURCE_VIDEO_CAPTURE.equals(res)) cam = true;
                    if (!cam || !onHome || !r.getOrigin().toString().startsWith("http://127.0.0.1:7780")) { r.deny(); return; }
                    String[] grant = new String[]{ PermissionRequest.RESOURCE_VIDEO_CAPTURE };
                    if (checkSelfPermission("android.permission.CAMERA") == android.content.pm.PackageManager.PERMISSION_GRANTED) { r.grant(grant); return; }
                    if (camReq != null) camReq.deny();
                    camReq = r;
                    requestPermissions(new String[]{"android.permission.CAMERA"}, CAM_REQ);
                }});
            }
            @Override public void onPermissionRequestCanceled(PermissionRequest r) { if (camReq == r) camReq = null; }
            @Override public boolean onShowFileChooser(WebView v, ValueCallback<Uri[]> cb, FileChooserParams p) {
                if (fileCb != null) { fileCb.onReceiveValue(null); fileCb = null; }
                if (!onHome) { cb.onReceiveValue(null); return true; }
                Intent i = new Intent(Intent.ACTION_GET_CONTENT).addCategory(Intent.CATEGORY_OPENABLE).setType("*/*");
                // Only real MIME types narrow the picker; extensions like .ygb would hide everything.
                java.util.ArrayList<String> mimes = new java.util.ArrayList<String>();
                boolean any = false;
                if (p.getAcceptTypes() != null) for (String t : p.getAcceptTypes()) for (String a : t.split(",")) {
                    a = a.trim();
                    if (a.isEmpty()) continue;
                    if (a.contains("/") && !a.equals("application/octet-stream")) mimes.add(a); else any = true;
                }
                if (!any && !mimes.isEmpty()) {
                    if (mimes.size() == 1) i.setType(mimes.get(0));
                    else i.putExtra(Intent.EXTRA_MIME_TYPES, mimes.toArray(new String[0]));
                }
                if (p.getMode() == FileChooserParams.MODE_OPEN_MULTIPLE) i.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true);
                fileCb = cb;
                try { startActivityForResult(Intent.createChooser(i, null), FILE_REQ); }
                catch (Exception e) { fileCb = null; cb.onReceiveValue(null); }
                return true;
            }
        });
        web.setDownloadListener(new DownloadListener() {
            public void onDownloadStart(String url, String ua, String cd, String mime, long len) { openOutside(Uri.parse(url)); }
        });
        setContentView(web);
        new Handler().postDelayed(new Runnable(){ public void run(){ loadHome(web, 0); }}, 800);
    }

    void vpnResult(boolean ok) {
        if (web != null) web.evaluateJavascript("window.ygVpnResult && window.ygVpnResult(" + ok + ")", null);
    }

    @Override protected void onActivityResult(int code, int res, Intent data) {
        if (code == FILE_REQ) {
            ValueCallback<Uri[]> cb = fileCb; fileCb = null;
            if (cb == null) return;
            Uri[] r = null;
            if (res == RESULT_OK && data != null) {
                if (data.getClipData() != null) {
                    int n = data.getClipData().getItemCount();
                    r = new Uri[n];
                    for (int k = 0; k < n; k++) r[k] = data.getClipData().getItemAt(k).getUri();
                } else if (data.getData() != null) r = new Uri[]{ data.getData() };
            }
            cb.onReceiveValue(r);
            return;
        }
        if (code != 2) return;
        if (res == RESULT_OK) { startService(new Intent(this, YggVpn.class)); vpnResult(true); }
        else { getSharedPreferences("ygg", 0).edit().putBoolean("vpn", false).apply(); vpnResult(false); }
    }

    @Override public void onRequestPermissionsResult(int code, String[] p, int[] r) {
        if (code == CAM_REQ) {
            PermissionRequest q = camReq; camReq = null;
            if (q == null) return;
            if (r.length > 0 && r[0] == android.content.pm.PackageManager.PERMISSION_GRANTED) q.grant(new String[]{ PermissionRequest.RESOURCE_VIDEO_CAPTURE });
            else q.deny();
            return;
        }
        if (web != null) web.evaluateJavascript("window.ygNotifResult && window.ygNotifResult('" + notifState() + "')", null);
    }

    @Override public void onBackPressed() {
        web.evaluateJavascript("window.ygBack ? window.ygBack() : false", new ValueCallback<String>() {
            public void onReceiveValue(String v) { if (!"true".equals(v)) moveTaskToBack(true); }
        });
    }
}
