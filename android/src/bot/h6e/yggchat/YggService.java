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
        return START_STICKY;
    }

    private static Thread watcher;
    private int notifId = 100;

    // Follows the node's event stream and posts a notification for each incoming
    // message while the app is not in the foreground (works with the UI closed).
    private synchronized void startWatcher() {
        if (watcher != null && watcher.isAlive()) return;
        watcher = new Thread(new Runnable() { public void run() {
            while (running) {
                try {
                    HttpURLConnection c = (HttpURLConnection) new URL("http://127.0.0.1:7780/api/events").openConnection();
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
            if (m.optBoolean("out") || m.has("sys") && !m.optString("sys").isEmpty()) return;
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

    private synchronized void startNode() {
        if (proc != null && proc.isAlive()) return;
        try {
            String bin = getApplicationInfo().nativeLibraryDir + "/libyggchat.so";
            File data = new File(getFilesDir(), "yggchat");
            data.mkdirs();
            ProcessBuilder pb = new ProcessBuilder(bin, "-data", data.getAbsolutePath(), "-ui", "127.0.0.1:7780");
            pb.environment().put("HOME", getFilesDir().getAbsolutePath());
            pb.environment().put("YGGCHAT_SUPERVISED", "1");
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
        if (proc != null) proc.destroy();
        proc = null;
        super.onDestroy();
    }
}
