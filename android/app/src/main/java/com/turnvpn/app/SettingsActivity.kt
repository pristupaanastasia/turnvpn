package com.turnvpn.app

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.os.Bundle
import android.widget.EditText
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import com.google.android.material.button.MaterialButton

// SettingsActivity holds the configuration you set once: the VPS endpoint, shared
// secret, server WireGuard public key and optional DTLS identity, plus this device's
// WireGuard public key (read-only, to register in the server's peers file). The main
// screen keeps only the per-session call fields.
class SettingsActivity : AppCompatActivity() {

    private lateinit var peer: EditText
    private lateinit var secret: EditText
    private lateinit var serverPub: EditText
    private lateinit var identity: EditText
    private lateinit var devicePubkey: TextView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_settings)

        peer = findViewById(R.id.peer)
        secret = findViewById(R.id.secret)
        serverPub = findViewById(R.id.server_pub)
        identity = findViewById(R.id.identity)
        devicePubkey = findViewById(R.id.device_pubkey)

        Prefs.ensureWgKeypair(this)
        loadPrefs()

        findViewById<MaterialButton>(R.id.copyPubkey).setOnClickListener {
            val pub = Prefs.of(this).getString("wgpub", "").orEmpty()
            (getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager)
                .setPrimaryClip(ClipData.newPlainText("wg pubkey", pub))
            Toast.makeText(this, "Ключ устройства скопирован", Toast.LENGTH_SHORT).show()
        }
        findViewById<MaterialButton>(R.id.saveSettings).setOnClickListener {
            savePrefs()
            Toast.makeText(this, "Сохранено", Toast.LENGTH_SHORT).show()
            finish()
        }
    }

    // Persist even if the user just backs out instead of tapping Save.
    override fun onPause() {
        super.onPause()
        savePrefs()
    }

    private fun loadPrefs() {
        val p = Prefs.of(this)
        peer.setText(p.getString("peer", ""))
        secret.setText(p.getString("secret", ""))
        serverPub.setText(p.getString("server_pub", ""))
        identity.setText(p.getString("identity", ""))
        devicePubkey.text = p.getString("wgpub", "")
    }

    private fun savePrefs() {
        Prefs.of(this).edit()
            .putString("peer", peer.text.toString().trim())
            .putString("secret", secret.text.toString().trim())
            .putString("server_pub", serverPub.text.toString().trim())
            .putString("identity", identity.text.toString().trim())
            .apply()
    }
}
