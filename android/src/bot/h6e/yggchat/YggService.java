package bot.h6e.yggchat;

import android.app.*;
import android.content.Intent;
import android.os.Build;
import android.os.IBinder;
import java.io.File;

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
        return START_STICKY;
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
