package bot.h6e.yggchat;

import android.app.Activity;
import android.content.Intent;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.webkit.*;

public class MainActivity extends Activity {
    private WebView web;
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
        web = new WebView(this);
        web.setBackgroundColor(0xff0b0f0b);
        WebSettings ws = web.getSettings();
        ws.setJavaScriptEnabled(true);
        ws.setDomStorageEnabled(true);
        web.addJavascriptInterface(new Object() {
            // Screen security (FLAG_SECURE): blocks screenshots and hides the app in recents.
            @JavascriptInterface public void setSecure(final boolean on) {
                runOnUiThread(new Runnable() { public void run() {
                    if (on) getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_SECURE);
                    else getWindow().clearFlags(android.view.WindowManager.LayoutParams.FLAG_SECURE);
                }});
            }
            // Notifications: WebView has no Web Notification API, so the service posts them natively.
            @JavascriptInterface public String notifState() { return MainActivity.this.notifState(); }
            @JavascriptInterface public void requestNotif() {
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
            @JavascriptInterface public void setNotif(boolean on, boolean hide) {
                getSharedPreferences("ygg", 0).edit().putBoolean("notif", on).putBoolean("hide", hide).apply();
            }
        }, "YggAndroid");
        web.setWebViewClient(new WebViewClient() {
            @Override public void onReceivedError(WebView v, WebResourceRequest r, WebResourceError e) {
                if (r.isForMainFrame()) { final WebView vv = v; new Handler().postDelayed(new Runnable(){ public void run(){ vv.loadUrl("http://127.0.0.1:7780/"); }}, 700); }
            }
        });
        setContentView(web);
        new Handler().postDelayed(new Runnable(){ public void run(){ web.loadUrl("http://127.0.0.1:7780/"); }}, 800);
    }

    @Override public void onRequestPermissionsResult(int code, String[] p, int[] r) {
        if (web != null) web.evaluateJavascript("window.ygNotifResult && window.ygNotifResult('" + notifState() + "')", null);
    }

    @Override public void onBackPressed() {
        web.evaluateJavascript("window.ygBack ? window.ygBack() : false", new ValueCallback<String>() {
            public void onReceiveValue(String v) { if (!"true".equals(v)) moveTaskToBack(true); }
        });
    }
}
