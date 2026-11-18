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
// Go core (tun2socks -> turnvpn tunnel). TCP only; see the Go side for the UDP note.
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
        Thread { startVpn(intent) }.start()
        return START_STICKY
    }

    private fun startVpn(intent: Intent) {
        try {
            val builder = Builder()
                .setSession("turnvpn")
                .setMtu(1500)
                .addAddress("10.0.0.2", 32)
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

            Turnvpn.start(
                pfd.fd.toLong(),
                1500L,
                intent.getStringExtra(EXTRA_PEER) ?: "",
                intent.getStringExtra(EXTRA_SECRET) ?: "",
                intent.getStringExtra(EXTRA_LINK) ?: "",
                intent.getStringExtra(EXTRA_TOKEN) ?: "",
                intent.getStringExtra(EXTRA_APPKEY) ?: DEFAULT_APPKEY,
            )
            Log.i(TAG, "turnvpn started")
        } catch (e: Exception) {
            Log.e(TAG, "start failed: ${e.message}", e)
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
            .setContentTitle("turnvpn")
            .setContentText("VPN active")
            .setSmallIcon(android.R.drawable.ic_lock_lock)
            .setOngoing(true)
            .build()
        startForeground(1, notif)
    }

    companion object {
        private const val TAG = "turnvpn"
        const val ACTION_STOP = "com.turnvpn.app.STOP"
        const val EXTRA_PEER = "peer"
        const val EXTRA_SECRET = "secret"
        const val EXTRA_LINK = "link"
        const val EXTRA_TOKEN = "token"
        const val EXTRA_APPKEY = "appkey"
        const val DEFAULT_APPKEY = "CGMMEJLGDIHBABABA"
    }
}
