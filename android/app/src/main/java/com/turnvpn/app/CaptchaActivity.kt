package com.turnvpn.app

import android.annotation.SuppressLint
import android.app.Activity
import android.content.Intent
import android.os.Bundle
import android.webkit.JavascriptInterface
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Toast

// CaptchaActivity opens the real VK call-join page in a WebView. The person joins the
// call (and solves the "not a robot" captcha if it appears) exactly as in a desktop
// browser; a small injected hook reads the per-call token (anonymToken) out of the
// fb.do join request and hands it back. This is the human-in-the-loop path: the
// person solves the captcha, the app only reads the resulting token.
class CaptchaActivity : Activity() {

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val link = intent.getStringExtra(EXTRA_LINK)?.trim().orEmpty()
        if (link.isEmpty()) {
            Toast.makeText(this, "Enter the VK call link first", Toast.LENGTH_LONG).show()
            finish()
            return
        }

        val web = WebView(this)
        setContentView(web)
        web.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            // Ask for the desktop web client (the one whose fb.do flow we read).
            userAgentString =
                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
                "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
        }
        web.addJavascriptInterface(Bridge(), "AndroidBridge")
        web.webViewClient = object : WebViewClient() {
            override fun onPageFinished(view: WebView, url: String) {
                // Arm the token hook on every page (lobby, then the joined call).
                view.evaluateJavascript(HOOK_JS, null)
            }
        }
        web.loadUrl("https://vk.ru/call/join/$link")
    }

    // Bridge receives the captured token from the injected JS.
    inner class Bridge {
        @JavascriptInterface
        fun onToken(token: String) {
            if (token.isBlank()) return
            runOnUiThread {
                setResult(Activity.RESULT_OK, Intent().putExtra(EXTRA_TOKEN, token))
                finish()
            }
        }
    }

    companion object {
        const val EXTRA_LINK = "link"
        const val EXTRA_TOKEN = "token"

        // Injected hook: patch XMLHttpRequest.prototype (+ fetch) and extract the
        // anonymToken from the vchat.joinConversationByLink fb.do request, then pass
        // it to the Android bridge. Mirrors the desktop capture.
        private const val HOOK_JS = """
(function(){
  var G=window;
  function dec(b){var s=b;try{if(typeof s==='string'&&s.indexOf('data=')===0)s=decodeURIComponent(s.split('&')[0].slice(5));}catch(e){}return String(s);}
  function extract(b){var s=dec(b);if(!/joinConversationByLink/.test(s))return null;var m=s.match(/"anonymToken"\s*:\s*"([^"]+)"/)||s.match(/(?:^|&)anonymToken=([^&]+)/);if(m){var v=m[1];try{v=decodeURIComponent(v);}catch(e){}return v;}return null;}
  function got(v){try{if(v&&G.AndroidBridge&&G.AndroidBridge.onToken)G.AndroidBridge.onToken(v);}catch(e){}}
  var X=G.XMLHttpRequest;
  if(X&&!X.prototype.__tvp){var o=X.prototype.open,s=X.prototype.send;
    X.prototype.open=function(m,u){this.__u=u;return o.apply(this,arguments);};
    X.prototype.send=function(b){if(this.__u&&/fb\.do/.test(this.__u)){var v=extract(b);if(v)got(v);}return s.apply(this,arguments);};
    try{Object.defineProperty(X.prototype,'__tvp',{value:true});}catch(e){}}
  var of=G.fetch;if(of&&!of.__tvp){G.fetch=function(i,init){try{var u=(typeof i==='string'?i:(i&&i.url))||'';if(/fb\.do/.test(u)&&init&&init.body){var v=extract(init.body);if(v)got(v);}}catch(e){}return of.apply(this,arguments);};G.fetch.__tvp=true;}
})();
"""
    }
}
