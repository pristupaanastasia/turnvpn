package com.turnvpn.app

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Bundle
import android.widget.Button
import android.widget.EditText
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity

class MainActivity : AppCompatActivity() {

    private lateinit var peer: EditText
    private lateinit var secret: EditText
    private lateinit var link: EditText
    private lateinit var token: EditText

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
        loadPrefs()

        findViewById<Button>(R.id.getToken).setOnClickListener {
            savePrefs()
            val l = link.text.toString().trim()
            if (l.isEmpty()) {
                Toast.makeText(this, "Enter the VK call link first", Toast.LENGTH_LONG).show()
            } else {
                capture.launch(Intent(this, CaptchaActivity::class.java).putExtra(CaptchaActivity.EXTRA_LINK, l))
            }
        }
        findViewById<Button>(R.id.connect).setOnClickListener {
            savePrefs()
            val prep = VpnService.prepare(this)
            if (prep != null) consent.launch(prep) else startVpn()
        }
        findViewById<Button>(R.id.disconnect).setOnClickListener {
            startService(Intent(this, TurnvpnService::class.java).setAction(TurnvpnService.ACTION_STOP))
        }
    }

    private fun startVpn() {
        val intent = Intent(this, TurnvpnService::class.java)
            .putExtra(TurnvpnService.EXTRA_PEER, peer.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_SECRET, secret.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_LINK, link.text.toString().trim())
            .putExtra(TurnvpnService.EXTRA_TOKEN, token.text.toString().trim())
        startService(intent)
    }

    private fun prefs() = getSharedPreferences("turnvpn", Context.MODE_PRIVATE)

    private fun loadPrefs() {
        val p = prefs()
        peer.setText(p.getString("peer", ""))
        secret.setText(p.getString("secret", ""))
        link.setText(p.getString("link", ""))
        token.setText(p.getString("token", ""))
    }

    private fun savePrefs() {
        prefs().edit()
            .putString("peer", peer.text.toString().trim())
            .putString("secret", secret.text.toString().trim())
            .putString("link", link.text.toString().trim())
            .putString("token", token.text.toString().trim())
            .apply()
    }
}
