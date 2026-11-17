// Package okapi builds and sends requests to the OK (Odnoklassniki) "fb.do" API
// that backs VK Звонки calls, reverse-engineered from the web client engine
// chunk 60828.js (see tools/fbdo-capture/signaling-reference.md).
//
// Every call is an application/x-www-form-urlencoded POST to {apiServer}/fb.do.
// The body carries method, format=JSON, application_key, an optional session_key,
// and the call's own parameters; object-valued parameters are JSON-encoded. There
// is no request signature (no sig/md5) — auth is the session_key alone — which is
// why this reproduces cleanly outside a browser.
package okapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Params is a single call's parameters. String values are sent as-is; any other
// value is JSON-encoded first, matching the client's object handling.
type Params map[string]any

// BuildForm assembles the url-encoded request body for an OK API call, mirroring
// the client's request builder (sQ in chunk 60828.js):
//
//   - sets method and format=JSON,
//   - sets application_key unless the caller already provided one,
//   - sets session_key when non-empty (skipped for pre-auth calls like
//     auth.anonymLogin, which pass an empty sessionKey),
//   - JSON-encodes non-string parameter values,
//   - encodes each pair as key=encodeURIComponent(value).
//
// Keys are emitted in sorted order for determinism; the server does not depend on
// order (there is no signature).
func BuildForm(method, appKey, sessionKey string, p Params) (string, error) {
	fields := make(map[string]string, len(p)+4)
	for k, v := range p {
		s, err := stringify(v)
		if err != nil {
			return "", fmt.Errorf("okapi: param %q: %w", k, err)
		}
		fields[k] = s
	}
	fields["method"] = method
	fields["format"] = "JSON"
	if _, ok := fields["application_key"]; !ok {
		fields["application_key"] = appKey
	}
	if sessionKey != "" {
		fields["session_key"] = sessionKey
	}

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(encodeURIComponent(fields[k]))
	}
	return b.String(), nil
}

// stringify returns strings unchanged and JSON-encodes everything else, as the
// client does for object-valued parameters.
func stringify(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// encodeURIComponent percent-encodes s exactly like JavaScript's
// encodeURIComponent: the unreserved set A-Za-z0-9 and -_.!~*'() pass through,
// every other byte of the UTF-8 encoding becomes %XX (uppercase hex). It differs
// from url.QueryEscape, which encodes some of those marks and uses '+' for space.
func encodeURIComponent(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isURIComponentUnreserved(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}

func isURIComponentUnreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}
