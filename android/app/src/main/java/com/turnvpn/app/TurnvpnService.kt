package com.turnvpn.app

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.os.ParcelFileDescriptor
import android.util.Log
import androidx.core.app.NotificationCompat
import com.turnvpn.turnvpn.Turnvpn

// TurnvpnService establishes a VPN tun interface and hands its file descriptor to the
// Go core, which runs a WireGuard device on it over the turnvpn transport
// (TURN + PSK-DTLS) to the VPS exit. WireGuard routes the whole device (TCP, UDP,
// QUIC); DNS resolves at the exit.
class TurnvpnService : VpnService() {

    private var tun: ParcelFileDescriptor? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopVpn()
            return START_NOT_STICKY
        }
        if (intent == null) {
            stopSelf()
            return START_NOT_STICKY
        }
        startForegroundNotif()
        VpnState.set(VpnState.S.CONNECTING)
        Thread { startVpn(intent) }.start()
        return START_STICKY
    }

    private fun startVpn(intent: Intent) {
        var goFd = -1
        try {
            val wgAddr = intent.getStringExtra(EXTRA_WGADDR)?.ifBlank { null } ?: DEFAULT_WGADDR
            val builder = Builder()
                .setSession("turnvpn")
                // WireGuard rides inside DTLS inside a TURN datagram, so keep the inner
                // MTU small to leave room for all the encapsulation.
                .setMtu(1280)
                .addAddress(wgAddr, 32)
                .addDnsServer("1.1.1.1")
                .addRoute("0.0.0.0", 0)
            // Keep this app out of the tunnel so control/credential traffic (and
            // loops) don't go through it.
            try {
                builder.addDisallowedApplication(packageName)
            } catch (_: Exception) {
            }

            val pfd = builder.establish() ?: throw IllegalStateException("establish() returned null")
            tun = pfd
            // Hand Go an independent dup of the tun fd. Passing pfd.fd directly makes
            // both the ParcelFileDescriptor and tun2socks claim ownership of the same
            // fd, and Android's fdsan then aborts the whole process (on connect and on
            // disconnect). A dup is a separate fd on the same tun: Go closes its fd, we
            // close ours — no ownership clash.
            goFd = pfd.dup().detachFd()

            Turnvpn.start(
                goFd.toLong(),
                1280L,
                intent.getStringExtra(EXTRA_PEER) ?: "",
                intent.getStringExtra(EXTRA_SECRET) ?: "",
                intent.getStringExtra(EXTRA_LINK) ?: "",
                intent.getStringExtra(EXTRA_TOKEN) ?: "",
                intent.getStringExtra(EXTRA_APPKEY) ?: DEFAULT_APPKEY,
                intent.getStringExtra(EXTRA_WGKEY) ?: "",
                intent.getStringExtra(EXTRA_WGSERVERPUB) ?: "",
                wgAddr,
                intent.getStringExtra(EXTRA_IDENTITY) ?: "",
            )
            goFd = -1 // ownership handed to Go; the engine closes it on Stop()
            Log.i(TAG, "turnvpn started")
            VpnState.set(VpnState.S.ON)
            updateNotif("Connected")
        } catch (e: Exception) {
            Log.e(TAG, "start failed: ${e.message}", e)
            // Go never adopted the dup (setup failed before the engine) — close it.
            if (goFd >= 0) {
                try {
                    ParcelFileDescriptor.adoptFd(goFd).close()
                } catch (_: Exception) {
                }
            }
            stopVpn()
        }
    }

    private fun stopVpn() {
        try {
            Turnvpn.stop()
        } catch (_: Exception) {
        }
        try {
            tun?.close()
        } catch (_: Exception) {
        }
        tun = null
        VpnState.set(VpnState.S.OFF)
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    override fun onDestroy() {
        stopVpn()
        super.onDestroy()
    }

    private fun startForegroundNotif() {
        val channelId = "turnvpn"
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val nm = getSystemService(NotificationManager::class.java)
            nm.createNotificationChannel(
                NotificationChannel(channelId, "turnvpn", NotificationManager.IMPORTANCE_LOW)
            )
        }
        val notif: Notification = NotificationCompat.Builder(this, channelId)
            .setContentTitle("HolubVPN")
            .setContentText("VPN active")
            .setSmallIcon(R.drawable.ic_stat_turnvpn)
            .setOngoing(true)
            .build()
        startForeground(1, notif)
    }

    private fun updateNotif(text: String) {
        val notif: Notification = NotificationCompat.Builder(this, "turnvpn")
            .setContentTitle("HolubVPN")
            .setContentText(text)
            .setSmallIcon(R.drawable.ic_stat_turnvpn)
            .setOngoing(true)
            .build()
        getSystemService(NotificationManager::class.java).notify(1, notif)
    }

    companion object {
        private const val TAG = "turnvpn"
        const val ACTION_STOP = "com.turnvpn.app.STOP"
        const val EXTRA_PEER = "peer"
        const val EXTRA_SECRET = "secret"
        const val EXTRA_LINK = "link"
        const val EXTRA_TOKEN = "token"
        const val EXTRA_APPKEY = "appkey"
        const val EXTRA_WGKEY = "wgkey"
        const val EXTRA_WGSERVERPUB = "wgserverpub"
        const val EXTRA_WGADDR = "wgaddr"
        const val EXTRA_IDENTITY = "identity"
        const val DEFAULT_APPKEY = "CGMMEJLGDIHBABABA"
        const val DEFAULT_WGADDR = "10.6.0.2"
    }
}
