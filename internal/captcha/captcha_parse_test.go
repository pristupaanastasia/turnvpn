package captcha

import (
	neturl "net/url"
	"strings"
	"testing"
)

func TestParseVkCaptchaErrorRedirectOnly(t *testing.T) {
	errData := map[string]interface{}{
		"error_code":         float64(14),
		"error_msg":          "Captcha need",
		"is_enabled_captcha": true,
		"redirect_uri":       "https://id.vk.ru/not_robot_captcha?domain=vk.com&session_token=session-token&variant=popup&blank=1",
	}
	got := ParseVkCaptchaError(errData)
	if got == nil {
		t.Fatal("expected captcha error")
	}
	if !got.IsCaptchaError() {
		t.Fatal("expected IsCaptchaError")
	}
	if !captchaHumanOnly(got) {
		t.Fatal("redirect-only challenge must be solved by a person")
	}
	if got.SessionToken != "session-token" {
		t.Fatalf("session token = %q", got.SessionToken)
	}
	if got.CaptchaSid != "" || got.CaptchaImg != "" {
		t.Fatalf("sid=%q img=%q, want empty", got.CaptchaSid, got.CaptchaImg)
	}

	mode, ok := captchaSolveModeForAttempt(0, captchaHumanOnly(got), true)
	if !ok || mode != captchaSolveModeManual {
		t.Fatalf("mode = %v ok=%v, want manual", mode, ok)
	}
	if _, ok := captchaSolveModeForAttempt(1, true, true); ok {
		t.Fatal("human-only challenge must not fall through to another solver")
	}
}

func TestRewriteCaptchaHTMLLeavesStaticBundle(t *testing.T) {
	page := `<html><head><script src="https://st.vk.ru/vkid/vkid-bff/dist/projects/notRobotCaptcha-web/entrypoints/entry.js"></script></head><body></body></html>`
	target, err := neturl.Parse("https://id.vk.ru/not_robot_captcha")
	if err != nil {
		t.Fatal(err)
	}
	out := rewriteCaptchaHTML(page, target)
	if !strings.Contains(out, `src="https://st.vk.ru/vkid/vkid-bff/dist/projects/notRobotCaptcha-web/entrypoints/entry.js"`) {
		t.Fatalf("entry script src changed:\n%s", out)
	}
	if !strings.Contains(out, "host === 'st.vk.ru'") || !strings.Contains(out, "host === 'st.vk.com'") {
		t.Fatal("injected rewrite must leave st.vk.ru and st.vk.com untouched")
	}
}

func TestParseVkCaptchaErrorRejectsNonCaptcha(t *testing.T) {
	got := ParseVkCaptchaError(map[string]interface{}{
		"error_code": float64(5),
		"error_msg":  "User authorization failed",
	})
	if got != nil {
		t.Fatalf("non-captcha error parsed as captcha: %+v", got)
	}
}
