package com.turnvpn.app

import android.os.Handler
import android.os.Looper

// VpnState is the single source of truth for the connection status, shared in-process
// between TurnvpnService (which drives the tunnel) and MainActivity (which renders it).
object VpnState {
    enum class S { OFF, CONNECTING, ON }

    @Volatile
    var state: S = S.OFF
        private set

    // Set by MainActivity while it is visible; always invoked on the main thread.
    var listener: ((S) -> Unit)? = null

    private val main = Handler(Looper.getMainLooper())

    fun set(s: S) {
        state = s
        main.post { listener?.invoke(s) }
    }
}
