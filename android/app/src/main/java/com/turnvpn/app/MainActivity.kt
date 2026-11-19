package com.turnvpn.app

import android.animation.ObjectAnimator
import android.animation.ValueAnimator
import android.app.Activity
import android.content.Intent
import android.content.res.ColorStateList
import android.net.VpnService
import android.os.Bundle
import android.view.animation.LinearInterpolator
import android.widget.EditText
import android.widget.ImageButton
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import com.google.android.material.button.MaterialButton

// MainActivity is the per-session screen: set/refresh the VK call link + token and
// connect. Everything you set once (VPS endpoint, shared secret, server WireGuard
// key, DTLS identity, this device's key) lives in SettingsActivity (gear icon).
class MainActivity : AppCompatActivity() {

    private lateinit var link: EditText
    private lateinit var token: EditText
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

        link = findViewById(R.id.link)
        token = findViewById(R.id.token)
        connectBtn = findViewById(R.id.connect)
        disconnectBtn = findViewById(R.id.disconnect)

        Prefs.ensureWgKeypair(this)
        loadPrefs()

        findViewById<ImageButton>(R.id.settings).setOnClickListener {
            startActivity(Intent(this, SettingsActivity::class.java))
        }
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
                    if (!settingsReady()) {
                        Toast.makeText(this, "Заполни настройки (⚙): VPS, secret, server key", Toast.LENGTH_LONG).show()
                        startActivity(Intent(this, SettingsActivity::class.java))
                        return@setOnClickListener
                    }
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

    // settingsReady checks the one-time config (in Settings) is filled before we try
    // to connect, so the user gets a clear nudge instead of an opaque failure.
    private fun settingsReady(): Boolean {
        val p = Prefs.of(this)
        return !p.getString("peer", "").isNullOrBlank() &&
            !p.getString("secret", "").isNullOrBlank() &&
            !p.getString("server_pub", "").isNullOrBlank() &&
            !p.getString("wgkey", "").isNullOrBlank()
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
        val p = Prefs.of(this)
        val intent = Intent(this, TurnvpnService::class.java)
            .putExtra(TurnvpnService.EXTRA_PEER, p.getString("peer", "").orEmpty().trim())
            .putExtra(TurnvpnService.EXTRA_SECRET, p.getString("secret", "").orEmpty().trim())
            .putExtra(TurnvpnService.EXTRA_LINK, link.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_TOKEN, token.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_WGKEY, p.getString("wgkey", "").orEmpty())
            .putExtra(TurnvpnService.EXTRA_WGSERVERPUB, p.getString("server_pub", "").orEmpty().trim())
            .putExtra(TurnvpnService.EXTRA_WGADDR, TurnvpnService.DEFAULT_WGADDR)
            .putExtra(TurnvpnService.EXTRA_IDENTITY, p.getString("identity", "").orEmpty().trim())
        startService(intent)
    }

    private fun loadPrefs() {
        val p = Prefs.of(this)
        link.setText(p.getString("link", ""))
        token.setText(p.getString("token", ""))
    }

    private fun savePrefs() {
        Prefs.of(this).edit()
            .putString("link", link.text.toString().trim())
            .putString("token", token.text.toString().trim())
            .apply()
    }
}
