// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package bot.h6e.yggchat;

import android.app.*;
import android.content.Intent;
import android.os.Build;
import android.os.IBinder;
import java.io.BufferedReader;
import java.io.File;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;
import org.json.JSONObject;

public class YggService extends Service {
    private static Process proc;
    private static volatile boolean running;

    @Override public IBinder onBind(Intent i) { return null; }

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        String ch = "ygg";
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (Build.VERSION.SDK_INT >= 26)
            nm.createNotificationChannel(new NotificationChannel(ch, "YggChat node", NotificationManager.IMPORTANCE_LOW));
        Notification.Builder b = Build.VERSION.SDK_INT >= 26 ? new Notification.Builder(this, ch) : new Notification.Builder(this);
        PendingIntent pi = PendingIntent.getActivity(this, 0, new Intent(this, MainActivity.class), PendingIntent.FLAG_IMMUTABLE);
        Notification n = b.setContentTitle("YggChat").setContentText("Yggdrasilノード稼働中")
            .setSmallIcon(android.R.drawable.stat_notify_chat).setContentIntent(pi).setOngoing(true).build();
        if (Build.VERSION.SDK_INT >= 34) startForeground(1, n, android.content.pm.ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        else startForeground(1, n);
        running = true;
        startNode();
        startWatcher();
        startLanWatch();
        return START_STICKY;
    }

    private static Thread watcher, lanWatch;
    private android.net.wifi.WifiManager.MulticastLock mlock;

    // LAN chat needs Wi-Fi multicast, which Android filters unless a
    // MulticastLock is held: hold it only while LAN chat is switched on.
    private synchronized void startLanWatch() {
        if (lanWatch != null && lanWatch.isAlive()) return;
        lanWatch = new Thread(new Runnable() { public void run() {
            while (running) {
                boolean on = false;
                try {
                    HttpURLConnection c = api(YggService.this, "/api/lan");
                    c.setConnectTimeout(3000); c.setReadTimeout(5000);
                    BufferedReader r = new BufferedReader(new InputStreamReader(c.getInputStream(), "UTF-8"));
                    StringBuilder sb = new StringBuilder(); String line;
                    while ((line = r.readLine()) != null) sb.append(line);
                    c.disconnect();
                    on = new JSONObject(sb.toString()).optBoolean("on");
                } catch (Exception e) { on = mlock != null && mlock.isHeld(); }
                setMulticast(on);
                try { Thread.sleep(5000); } catch (InterruptedException e) { return; }
            }
            setMulticast(false);
        }});
        lanWatch.setDaemon(true);
        lanWatch.start();
    }

    private synchronized void setMulticast(boolean on) {
        try {
            if (mlock == null) {
                android.net.wifi.WifiManager wm = (android.net.wifi.WifiManager) getApplicationContext().getSystemService(WIFI_SERVICE);
                mlock = wm.createMulticastLock("yggchat-lan");
                mlock.setReferenceCounted(false);
            }
            if (on && !mlock.isHeld()) mlock.acquire();
            if (!on && mlock.isHeld()) mlock.release();
        } catch (Exception e) { }
    }
    private int notifId = 100;

    // Follows the node's event stream and posts a notification for each incoming
    // message while the app is not in the foreground (works with the UI closed).
    private synchronized void startWatcher() {
        if (watcher != null && watcher.isAlive()) return;
        watcher = new Thread(new Runnable() { public void run() {
            while (running) {
                try {
                    HttpURLConnection c = api(YggService.this, "/api/events");
                    c.setReadTimeout(60000);
                    BufferedReader r = new BufferedReader(new InputStreamReader(c.getInputStream(), "UTF-8"));
                    String line;
                    while (running && (line = r.readLine()) != null) {
                        if (line.startsWith("data: ")) onEvent(line.substring(6));
                    }
                    c.disconnect();
                } catch (Exception e) { }
                try { Thread.sleep(3000); } catch (InterruptedException e) { return; }
            }
        }});
        watcher.setDaemon(true);
        watcher.start();
    }

    private void onEvent(String s) {
        try {
            JSONObject ev = new JSONObject(s);
            if (!"message".equals(ev.optString("type"))) return;
            JSONObject m = ev.getJSONObject("data");
            if (m.optBoolean("out") || m.optBoolean("hist") || m.has("sys") && !m.optString("sys").isEmpty()) return;
            android.content.SharedPreferences sp = getSharedPreferences("ygg", 0);
            if (!sp.getBoolean("notif", false) || MainActivity.visible) return;
            NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
            String ch = "msg";
            if (Build.VERSION.SDK_INT >= 26)
                nm.createNotificationChannel(new NotificationChannel(ch, "メッセージ", NotificationManager.IMPORTANCE_HIGH));
            String title = "YggChat", body = "新しいメッセージ";
            if (!sp.getBoolean("hide", false)) {
                String nick = m.optString("nick");
                String from = m.optString("from", m.optString("peer"));
                title = nick.isEmpty() ? from : nick;
                String t = m.optString("text");
                body = m.optString("img").isEmpty() ? t : ("📷 写真" + (t.isEmpty() ? "" : " " + t));
            }
            Notification.Builder b = Build.VERSION.SDK_INT >= 26 ? new Notification.Builder(this, ch) : new Notification.Builder(this);
            PendingIntent pi = PendingIntent.getActivity(this, 1, new Intent(this, MainActivity.class).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP), PendingIntent.FLAG_IMMUTABLE);
            b.setContentTitle(title).setContentText(body).setSmallIcon(android.R.drawable.stat_notify_chat)
                .setContentIntent(pi).setAutoCancel(true);
            if (Build.VERSION.SDK_INT < 26) b.setPriority(Notification.PRIORITY_HIGH).setDefaults(Notification.DEFAULT_ALL);
            nm.notify(notifId++, b.build());
        } catch (Exception e) { }
    }

    // The node's local API key (<data>/ui-key): every request must carry it,
    // because other apps on the device can reach 127.0.0.1 too.
    static String uiKey(android.content.Context ctx) {
        try {
            java.io.FileInputStream in = new java.io.FileInputStream(new File(new File(ctx.getFilesDir(), "yggchat"), "ui-key"));
            byte[] b = new byte[128]; int n = in.read(b); in.close();
            return n > 0 ? new String(b, 0, n, "UTF-8").trim() : "";
        } catch (Exception e) { return ""; }
    }

    // Checks that 127.0.0.1:7780 is really our node before the key is sent:
    // while the node is down (first start, restarts) any other app can bind
    // the port and would otherwise collect the key from our requests or the
    // WebView launch URL. The node proves it knows the key with
    // HMAC-SHA256(key, "yggchat-ui-proof\n" + nonce) at GET /api/proof.
    static boolean nodeOK(String key) {
        if (key == null || key.isEmpty()) return false;
        // The proof is a separate connection: it only means something for the
        // next request if nobody else can take the port in between, i.e.
        // while this app holds it (holdUiPort).
        if (!uiHeld()) { ensureUiPort(); return false; }
        HttpURLConnection c = null;
        try {
            byte[] nb = new byte[16];
            new java.security.SecureRandom().nextBytes(nb);
            String n = hex(nb);
            c = (HttpURLConnection) new URL("http://127.0.0.1:7780/api/proof?n=" + n).openConnection();
            c.setConnectTimeout(2000); c.setReadTimeout(3000);
            c.setInstanceFollowRedirects(false);
            if (c.getResponseCode() != 200) return false;
            java.io.InputStream in = c.getInputStream();
            byte[] buf = new byte[256]; int len = 0, r;
            while (len < buf.length && (r = in.read(buf, len, buf.length - len)) > 0) len += r;
            String mac = new JSONObject(new String(buf, 0, len, "UTF-8")).optString("mac");
            javax.crypto.Mac m = javax.crypto.Mac.getInstance("HmacSHA256");
            m.init(new javax.crypto.spec.SecretKeySpec(key.getBytes("UTF-8"), "HmacSHA256"));
            String want = hex(m.doFinal(("yggchat-ui-proof\n" + n).getBytes("UTF-8")));
            return java.security.MessageDigest.isEqual(want.getBytes("UTF-8"), mac.getBytes("UTF-8"));
        } catch (Exception e) {
            return false;
        } finally { if (c != null) c.disconnect(); }
    }

    static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder();
        for (byte x : b) sb.append(String.format("%02x", x & 0xff));
        return sb.toString();
    }

    static HttpURLConnection api(android.content.Context ctx, String path) throws java.io.IOException {
        String k = uiKey(ctx);
        if (!nodeOK(k)) throw new java.io.IOException("node not ready or not ours");
        HttpURLConnection c = (HttpURLConnection) new URL("http://127.0.0.1:7780" + path).openConnection();
        c.setInstanceFollowRedirects(false);
        c.setRequestProperty("X-YggChat-Key", k);
        return c;
    }

    // The VPN was revoked (another VPN app took over, or the permission is
    // gone): the node must leave VPN mode, or it keeps using the OS stack
    // with no TUN behind it and chat stops working until the user toggles
    // the setting by hand. Retries in the background until the node answers.
    static void leaveVpnMode(final android.content.Context ctx) {
        final android.content.Context app = ctx.getApplicationContext();
        new Thread(new Runnable() { public void run() {
            for (int i = 0; i < 60; i++) {
                try {
                    HttpURLConnection g = api(app, "/api/vpn");
                    g.setConnectTimeout(2000); g.setReadTimeout(3000);
                    java.util.Scanner sc = new java.util.Scanner(g.getInputStream(), "UTF-8").useDelimiter("\\A");
                    JSONObject st = new JSONObject(sc.hasNext() ? sc.next() : "{}");
                    g.disconnect();
                    if (!st.optBoolean("on")) return;
                    if (app.getSharedPreferences("ygg", 0).getBoolean("vpn", false)) return; // turned back on meanwhile
                    HttpURLConnection c = api(app, "/api/vpn");
                    c.setConnectTimeout(2000); c.setReadTimeout(3000);
                    c.setRequestMethod("POST");
                    c.setDoOutput(true);
                    c.setRequestProperty("Content-Type", "application/json");
                    c.getOutputStream().write("{\"On\":false}".getBytes("UTF-8"));
                    int code = c.getResponseCode();
                    c.disconnect();
                    if (code == 200) return;
                } catch (Exception e) { }
                try { Thread.sleep(2000); } catch (InterruptedException e) { return; }
            }
        }}).start();
    }

    // ---------- the UI port, held by the app (v0.19.4) ----------
    // The node used to bind 127.0.0.1:7780 itself, so the port was free
    // whenever it was down (first start, restartSelf, crash) and any app could
    // take it. A proof check before each request still left a window: the port
    // could change hands between the check and the request (the WebView
    // re-checked at most every 300 ms). Now this process binds the port once
    // and keeps it for its whole life, and hands the listening socket to each
    // node it starts over a same-uid unix socket. Connections made while the
    // node restarts wait in the backlog instead of reaching another app, and
    // nobody else can bind the port while the WebView (same process) runs.
    private static volatile java.io.FileDescriptor uiFd;
    private static String uiFdName;
    private static Thread uiPortThread, uiFdServer;

    static boolean uiHeld() { return uiFd != null; }

    private static synchronized boolean holdUiPort() {
        if (uiFd != null) return true;
        java.io.FileDescriptor fd = null;
        try {
            fd = android.system.Os.socket(android.system.OsConstants.AF_INET, android.system.OsConstants.SOCK_STREAM, 0);
            android.system.Os.setsockoptInt(fd, android.system.OsConstants.SOL_SOCKET, android.system.OsConstants.SO_REUSEADDR, 1);
            android.system.Os.bind(fd, java.net.InetAddress.getByAddress(new byte[]{127, 0, 0, 1}), 7780);
            android.system.Os.listen(fd, 128);
            uiFd = fd;
            return true;
        } catch (Exception e) {
            if (fd != null) { try { android.system.Os.close(fd); } catch (Exception x) { } }
            return false;
        }
    }

    // Keeps trying to bind the port (someone else may hold it right now).
    static synchronized void ensureUiPort() {
        if (uiFd != null || (uiPortThread != null && uiPortThread.isAlive())) return;
        uiPortThread = new Thread(new Runnable() { public void run() {
            while (!holdUiPort()) { try { Thread.sleep(300); } catch (InterruptedException e) { return; } }
        }});
        uiPortThread.setDaemon(true);
        uiPortThread.start();
    }

    // Serves the held socket to our node (YGGCHAT_UIFD=<name>, an abstract
    // unix socket with a random name; both ends check the peer's uid).
    private static synchronized String uiFdServer() {
        if (uiFdName != null && uiFdServer != null && uiFdServer.isAlive()) return uiFdName;
        byte[] nb = new byte[12];
        new java.security.SecureRandom().nextBytes(nb);
        final String name = "yggchat-ui-" + hex(nb);
        final android.net.LocalServerSocket srv;
        try { srv = new android.net.LocalServerSocket(name); } catch (java.io.IOException e) { return null; }
        uiFdName = name;
        uiFdServer = new Thread(new Runnable() { public void run() {
            while (true) {
                android.net.LocalSocket s = null;
                try {
                    s = srv.accept();
                    if (s.getPeerCredentials().getUid() != android.os.Process.myUid()) continue;
                    for (int i = 0; i < 10 && !holdUiPort(); i++) Thread.sleep(200);
                    java.io.FileDescriptor fd = uiFd;
                    if (fd == null) continue; // the node asks again
                    s.setFileDescriptorsForSend(new java.io.FileDescriptor[]{ fd });
                    s.getOutputStream().write(1);
                    s.setFileDescriptorsForSend(null);
                    s.setSoTimeout(5000);
                    s.getInputStream().read();
                } catch (Exception e) {
                } finally { if (s != null) { try { s.close(); } catch (Exception e) { } } }
            }
        }});
        uiFdServer.setDaemon(true);
        uiFdServer.start();
        return name;
    }

    private synchronized void startNode() {
        if (proc != null && proc.isAlive()) return;
        try {
            String bin = getApplicationInfo().nativeLibraryDir + "/libyggchat.so";
            File data = new File(getFilesDir(), "yggchat");
            data.mkdirs();
            ProcessBuilder pb = new ProcessBuilder(bin, "-data", data.getAbsolutePath(), "-ui", "127.0.0.1:7780");
            pb.environment().put("HOME", getFilesDir().getAbsolutePath());
            pb.environment().put("YGGCHAT_SUPERVISED", "1");
            ensureUiPort();
            String fdName = uiFdServer();
            if (fdName != null) pb.environment().put("YGGCHAT_UIFD", fdName);
            pb.redirectErrorStream(true);
            pb.redirectOutput(new File(data, "node.log"));
            final Process p = pb.start();
            proc = p;
            // The node exits to restart itself (e.g. after "new ID"): bring it back.
            new Thread(new Runnable() { public void run() {
                try { p.waitFor(); } catch (InterruptedException e) { return; }
                if (!running) return;
                try { Thread.sleep(500); } catch (InterruptedException e) { return; }
                startNode();
            }}).start();
        } catch (Exception e) { e.printStackTrace(); }
    }

    @Override public void onDestroy() {
        running = false;
        setMulticast(false);
        if (proc != null) proc.destroy();
        proc = null;
        super.onDestroy();
    }
}
