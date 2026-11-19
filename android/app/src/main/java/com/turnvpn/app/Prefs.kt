package com.turnvpn.app

import android.content.Context
import android.content.SharedPreferences
import android.util.Log
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import com.turnvpn.turnvpn.Turnvpn

// Prefs is the single Keystore-backed encrypted store shared by MainActivity and
// SettingsActivity, so the shared secret and this device's WireGuard private key are
// never written in plaintext on a rooted or imaged device.
//
// Keys: peer, secret, server_pub, identity (set in Settings); link, token (per
// session, set on Main); wgkey/wgpub (this device's WireGuard keypair).
object Prefs {
    fun of(ctx: Context): SharedPreferences {
        val master = MasterKey.Builder(ctx)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .build()
        return EncryptedSharedPreferences.create(
            ctx,
            "turnvpn_secure",
            master,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    // ensureWgKeypair generates this device's WireGuard keypair once (hex) and stores
    // the private half; the public half ("wgpub") is shown in Settings so the operator
    // can add it to the server's peers file.
    fun ensureWgKeypair(ctx: Context) {
        val p = of(ctx)
        if (!p.getString("wgkey", "").isNullOrBlank()) return
        try {
            val parts = Turnvpn.generateWGKeypair().trim().split(" ") // "privateHex publicHex"
            if (parts.size == 2) {
                p.edit().putString("wgkey", parts[0]).putString("wgpub", parts[1]).apply()
            }
        } catch (e: Exception) {
            Log.e("turnvpn", "wg keypair generation failed: ${e.message}", e)
        }
    }
}
