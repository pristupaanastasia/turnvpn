package com.turnvpn.app

import android.animation.ObjectAnimator
import android.animation.ValueAnimator
import android.app.Activity
import android.content.Intent
import android.content.SharedPreferences
import android.content.res.ColorStateList
import android.net.VpnService
import android.os.Bundle
import android.util.Log
import android.view.animation.LinearInterpolator
import android.widget.EditText
import android.widget.TextView
import android.widget.Toast
import com.turnvpn.turnvpn.Turnvpn
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import com.google.android.material.button.MaterialButton

class MainActivity : AppCompatActivity() {

    private lateinit var peer: EditText
    private lateinit var secret: EditText
    private lateinit var link: EditText
    private lateinit var token: EditText
    private lateinit var serverPub: EditText
    private lateinit var identity: EditText
    private lateinit var devicePubkey: TextView

    private lateinit var connectBtn: MaterialButton
    private lateinit var disconnectBtn: MaterialButton
    private var pulse: ObjectAnimator? = null

    // VpnService.prepare consent dialog; on OK we launch the service.
    private val consent = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { res ->
        if (res.resultCode == Activity.RESULT_OK) startVpn()
    }

    // Captcha/join WebView; on success it returns the captured per-call token.
    private val capture = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { res ->
        if (res.resultCode == Activity.RESULT_OK) {
            val t = res.data?.getStringExtra(CaptchaActivity.EXTRA_TOKEN).orEmpty()
            if (t.isNotBlank()) {
                token.setText(t)
                savePrefs()
                Toast.makeText(this, "Token captured", Toast.LENGTH_SHORT).show()
            }
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        peer = findViewById(R.id.peer)
        secret = findViewById(R.id.secret)
        link = findViewById(R.id.link)
        token = findViewById(R.id.token)
        serverPub = findViewById(R.id.server_pub)
        identity = findViewById(R.id.identity)
        devicePubkey = findViewById(R.id.device_pubkey)
        connectBtn = findViewById(R.id.connect)
        disconnectBtn = findViewById(R.id.disconnect)
        ensureWgKeypair()
        loadPrefs()

        findViewById<MaterialButton>(R.id.getToken).setOnClickListener {
            savePrefs()
            val l = link.text.toString().trim()
            if (l.isEmpty()) {
                Toast.makeText(this, "Enter the VK call link first", Toast.LENGTH_LONG).show()
            } else {
                capture.launch(Intent(this, CaptchaActivity::class.java).putExtra(CaptchaActivity.EXTRA_LINK, l))
            }
        }
        connectBtn.setOnClickListener {
            when (VpnState.state) {
                VpnState.S.ON -> Toast.makeText(this, "Already connected", Toast.LENGTH_SHORT).show()
                VpnState.S.CONNECTING -> Toast.makeText(this, "Connecting…", Toast.LENGTH_SHORT).show()
                VpnState.S.OFF -> {
                    savePrefs()
                    val prep = VpnService.prepare(this)
                    if (prep != null) consent.launch(prep) else startVpn()
                }
            }
        }
        disconnectBtn.setOnClickListener {
            startService(Intent(this, TurnvpnService::class.java).setAction(TurnvpnService.ACTION_STOP))
        }
    }

    override fun onResume() {
        super.onResume()
        VpnState.listener = { render(it) }
        VpnState.errorListener = { msg -> Toast.makeText(this, msg, Toast.LENGTH_LONG).show() }
        render(VpnState.state)
    }

    override fun onPause() {
        super.onPause()
        VpnState.listener = null
        VpnState.errorListener = null
    }

    // render reflects the connection status in the Connect button: a calm violet
    // "Connect" when off, "Connecting…" while establishing, and a green, gently
    // pulsing "Connected" once the tunnel is up.
    private fun render(s: VpnState.S) {
        when (s) {
            VpnState.S.OFF -> {
                stopPulse()
                connectBtn.text = "Connect"
                connectBtn.backgroundTintList =
                    ColorStateList.valueOf(ContextCompat.getColor(this, R.color.accent))
                connectBtn.setTextColor(ContextCompat.getColor(this, R.color.onConnected))
                connectBtn.isEnabled = true
                disconnectBtn.isEnabled = false
                disconnectBtn.alpha = 0.4f
            }
            VpnState.S.CONNECTING -> {
                stopPulse()
                connectBtn.text = "Connecting…"
                connectBtn.backgroundTintList =
                    ColorStateList.valueOf(ContextCompat.getColor(this, R.color.accent))
                connectBtn.setTextColor(ContextCompat.getColor(this, R.color.onConnected))
                connectBtn.isEnabled = true
                disconnectBtn.isEnabled = true
                disconnectBtn.alpha = 1f
            }
            VpnState.S.ON -> {
                connectBtn.text = "● Connected"
                connectBtn.backgroundTintList =
                    ColorStateList.valueOf(ContextCompat.getColor(this, R.color.connected))
                connectBtn.setTextColor(ContextCompat.getColor(this, R.color.onConnected))
                connectBtn.isEnabled = true
                disconnectBtn.isEnabled = true
                disconnectBtn.alpha = 1f
                startPulse()
            }
        }
    }

    private fun startPulse() {
        if (pulse?.isRunning == true) return
        pulse = ObjectAnimator.ofFloat(connectBtn, "alpha", 1f, 0.6f).apply {
            duration = 850
            repeatCount = ValueAnimator.INFINITE
            repeatMode = ValueAnimator.REVERSE
            interpolator = LinearInterpolator()
            start()
        }
    }

    private fun stopPulse() {
        pulse?.cancel()
        pulse = null
        connectBtn.alpha = 1f
    }

    private fun startVpn() {
        val intent = Intent(this, TurnvpnService::class.java)
            .putExtra(TurnvpnService.EXTRA_PEER, peer.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_SECRET, secret.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_LINK, link.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_TOKEN, token.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_WGKEY, prefs().getString("wgkey", ""))
            .putExtra(TurnvpnService.EXTRA_WGSERVERPUB, serverPub.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_WGADDR, TurnvpnService.DEFAULT_WGADDR)
            .putExtra(TurnvpnService.EXTRA_IDENTITY, identity.text.toString().trim())
        startService(intent)
    }

    // Keystore-backed encrypted storage for the VPS/secret/link/token, so the shared
    // secret is not readable in plaintext on a rooted or imaged device.
    private val securePrefs: SharedPreferences by lazy {
        val master = MasterKey.Builder(this)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .build()
        EncryptedSharedPreferences.create(
            this,
            "turnvpn_secure",
            master,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    private fun prefs() = securePrefs

    private fun loadPrefs() {
        val p = prefs()
        peer.setText(p.getString("peer", ""))
        secret.setText(p.getString("secret", ""))
        link.setText(p.getString("link", ""))
        token.setText(p.getString("token", ""))
        serverPub.setText(p.getString("server_pub", ""))
        identity.setText(p.getString("identity", ""))
        devicePubkey.text = "ключ этого устройства (в peers сервера):\n" + p.getString("wgpub", "")
    }

    private fun savePrefs() {
        prefs().edit()
            .putString("peer", peer.text.toString().trim())
            .putString("secret", secret.text.toString().trim())
            .putString("link", link.text.toString().trim())
            .putString("token", token.text.toString().trim())
            .putString("server_pub", serverPub.text.toString().trim())
            .putString("identity", identity.text.toString().trim())
            .apply()
    }

    // ensureWgKeypair generates this device's WireGuard keypair once and stores the
    // private half in the Keystore-backed prefs. The public half is shown so the
    // operator can add it to the server's peers file.
    private fun ensureWgKeypair() {
        val p = prefs()
        if (!p.getString("wgkey", "").isNullOrBlank()) return
        try {
            val pair = Turnvpn.generateWGKeypair() // "privateHex publicHex"
            val parts = pair.trim().split(" ")
            if (parts.size == 2) {
                p.edit().putString("wgkey", parts[0]).putString("wgpub", parts[1]).apply()
            }
        } catch (e: Exception) {
            Log.e("turnvpn", "wg keypair generation failed: ${e.message}", e)
        }
    }
}
