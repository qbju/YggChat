package bot.h6e.yggchat;

import android.content.Intent;
import android.net.LocalSocket;
import android.net.LocalSocketAddress;
import android.net.VpnService;
import android.os.ParcelFileDescriptor;
import android.system.OsConstants;
import java.io.FileDescriptor;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import org.json.JSONObject;

// Makes YggChat the device's Yggdrasil: a TUN for 200::/7 whose fd is handed to
// the Go node (abstract socket "yggchat-tun"), which pumps it into its core.
public class YggVpn extends VpnService {
    static final String STOP = "bot.h6e.yggchat.VPN_STOP";
    private static volatile boolean running;
    private static Thread loop;
    private ParcelFileDescriptor tun;
    private String tunAddr = "";

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent != null && STOP.equals(intent.getAction())) { shutdown(); stopSelf(); return START_NOT_STICKY; }
        running = true;
        if (loop == null || !loop.isAlive()) {
            loop = new Thread(new Runnable() { public void run() { work(); } });
            loop.setDaemon(true);
            loop.start();
        }
        return START_STICKY;
    }

    private void work() {
        while (running) {
            try {
                HttpURLConnection c = (HttpURLConnection) new URL("http://127.0.0.1:7780/api/vpn").openConnection();
                c.setConnectTimeout(2000); c.setReadTimeout(3000);
                InputStream in = c.getInputStream();
                java.util.Scanner sc = new java.util.Scanner(in, "UTF-8").useDelimiter("\\A");
                JSONObject st = new JSONObject(sc.hasNext() ? sc.next() : "{}");
                c.disconnect();
                String addr = st.optString("addr");
                if (st.optBoolean("on") && !addr.isEmpty()) {
                    if (tun == null || !addr.equals(tunAddr)) establish(addr, st.optInt("mtu", 1280));
                    if (tun != null && !st.optBoolean("tun")) handOver();
                } else if (tun != null) { closeTun(); }
            } catch (Exception e) { }
            try { Thread.sleep(3000); } catch (InterruptedException e) { return; }
        }
    }

    private void establish(String addr, int mtu) throws Exception {
        closeTun();
        Builder b = new Builder().setSession("YggChat (Yggdrasil)").setMtu(mtu)
            .addAddress(addr, 7).addRoute("200::", 7);
        b.allowFamily(OsConstants.AF_INET);
        b.allowBypass();
        android.app.PendingIntent pi = android.app.PendingIntent.getActivity(this, 2, new Intent(this, MainActivity.class), android.app.PendingIntent.FLAG_IMMUTABLE);
        b.setConfigureIntent(pi);
        tun = b.establish();
        tunAddr = tun != null ? addr : "";
    }

    private void handOver() throws Exception {
        LocalSocket s = new LocalSocket();
        try {
            s.connect(new LocalSocketAddress("yggchat-tun"));
            s.setFileDescriptorsForSend(new FileDescriptor[]{ tun.getFileDescriptor() });
            s.getOutputStream().write(1);
            s.getInputStream().read();
        } finally { s.close(); }
    }

    private void closeTun() {
        if (tun != null) { try { tun.close(); } catch (Exception e) { } }
        tun = null; tunAddr = "";
    }

    private void shutdown() {
        running = false;
        if (loop != null) loop.interrupt();
        closeTun();
    }

    @Override public void onRevoke() {
        getSharedPreferences("ygg", 0).edit().putBoolean("vpn", false).apply();
        shutdown();
        stopSelf();
    }

    @Override public void onDestroy() { shutdown(); super.onDestroy(); }
}
