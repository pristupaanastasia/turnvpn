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

    // Invoked on the main thread when a connect attempt fails, so the UI can show
    // the reason instead of failing silently.
    var errorListener: ((String) -> Unit)? = null

    private val main = Handler(Looper.getMainLooper())

    fun set(s: S) {
        state = s
        main.post { listener?.invoke(s) }
    }

    // fail moves to OFF and reports why; the message reaches the UI if it is visible.
    fun fail(message: String) {
        state = S.OFF
        main.post {
            errorListener?.invoke(message)
            listener?.invoke(S.OFF)
        }
    }
}
