// Command analyze reads a cap.json produced by tools/fbdo-capture/capture.js and
// turns it into a readable map of the VK/OK call signaling, so we can reverse the
// fb.do ("TopGun"-style base64) protocol by hand.
//
// It does NOT assume a format. For every captured request/response/WS frame it:
//   - prints an ordered, timestamped timeline;
//   - pretty-prints bodies that are JSON;
//   - finds long base64-looking runs, decodes them, hexdumps the head, and
//     extracts printable strings + flags signaling markers (SDP m=/a=/candidate,
//     ice-ufrag/pwd, fingerprint, turn:/stun:, STUN magic cookie 0x2112A442).
//
// Usage:
//
//	go run ./tools/fbdo-capture/analyze -in cap.json            # full timeline
//	go run ./tools/fbdo-capture/analyze -in cap.json -grep sdp  # only events whose decoded text hits a marker
//	go run ./tools/fbdo-capture/analyze -in cap.json -seq 7     # one event, full detail
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

type event struct {
	Seq      int    `json:"seq"`
	T        int    `json:"t"`
	Kind     string `json:"kind"`
	Dir      string `json:"dir"`
	URL      string `json:"url"`
	WsURL    string `json:"wsUrl"`
	Method   string `json:"method"`
	Status   int    `json:"status"`
	ReqBody  string `json:"reqBody"`
	RespBody string `json:"respBody"`
	ForSeq   int    `json:"forSeq"`
}

type capFile struct {
	T0     int64   `json:"t0"`
	Span   int     `json:"span"`
	Events []event `json:"events"`
}

var (
	b64Run  = regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}={0,2}`)
	printer = regexp.MustCompile(`[\x20-\x7e]{4,}`)
	markers = []struct {
		name string
		re   *regexp.Regexp
	}{
		{"SDP:m-line", regexp.MustCompile(`(?m)^m=(audio|video|application)`)},
		{"SDP:attr", regexp.MustCompile(`(?m)^a=(candidate|fingerprint|ice-ufrag|ice-pwd|setup|mid|rtpmap)`)},
		{"ice-ufrag", regexp.MustCompile(`ice-?ufrag`)},
		{"fingerprint", regexp.MustCompile(`fingerprint`)},
		{"candidate", regexp.MustCompile(`candidate:`)},
		{"turn/stun", regexp.MustCompile(`\b(turn|stun)s?:`)},
		{"credential", regexp.MustCompile(`credential|password|pwd`)},
		{"opus/vp8", regexp.MustCompile(`(?i)opus|VP8|H264|rtx`)},
	}
)

func main() {
	in := flag.String("in", "cap.json", "capture file from capture.js")
	grep := flag.String("grep", "", "only show events whose decoded content matches this (case-insensitive substring)")
	seqOnly := flag.Int("seq", 0, "show only this event seq, in full detail")
	maxHex := flag.Int("hex", 256, "max bytes to hexdump per decoded blob")
	flag.Parse()

	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	var cf capFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	sort.Slice(cf.Events, func(i, j int) bool { return cf.Events[i].Seq < cf.Events[j].Seq })

	fmt.Printf("== cap: %d events, span %dms ==\n\n", len(cf.Events), cf.Span)
	grepL := strings.ToLower(*grep)

	for _, e := range cf.Events {
		if *seqOnly != 0 && e.Seq != *seqOnly {
			continue
		}
		body := e.RespBody
		if e.Dir == "req" {
			body = e.ReqBody
		}
		dec := analyzeBody(body, *maxHex)
		if grepL != "" && !strings.Contains(strings.ToLower(e.URL+e.WsURL+body+dec.flat), grepL) {
			continue
		}
		where := e.URL
		if where == "" {
			where = e.WsURL
		}
		fmt.Printf("#%d @%dms  %s.%s  %s %s  st=%d\n", e.Seq, e.T, e.Kind, e.Dir, e.Method, trunc(where, 100), e.Status)
		if len(dec.hits) > 0 {
			fmt.Printf("   markers: %s\n", strings.Join(dec.hits, ", "))
		}
		detail := *seqOnly != 0
		printBody(body, dec, detail, *maxHex)
		fmt.Println()
	}
}

type decoded struct {
	hits   []string
	flat   string // all extracted text, for -grep
	blobs  []blob
	isJSON bool
	pretty string
}

type blob struct {
	src     string
	bytes   []byte
	strings []string
	hits    []string
}

func analyzeBody(body string, maxHex int) decoded {
	var d decoded
	if body == "" {
		return d
	}
	// JSON?
	var js any
	if json.Unmarshal([]byte(body), &js) == nil {
		d.isJSON = true
		if pp, err := json.MarshalIndent(js, "   ", "  "); err == nil {
			d.pretty = string(pp)
		}
	}
	// base64 blobs
	for _, m := range b64Run.FindAllString(body, -1) {
		raw, ok := tryB64(m)
		if !ok || len(raw) < 8 {
			continue
		}
		b := blob{src: trunc(m, 48), bytes: raw}
		for _, s := range printer.FindAllString(string(raw), -1) {
			b.strings = append(b.strings, s)
		}
		b.hits = scanMarkers(string(raw) + " " + strings.Join(b.strings, " "))
		d.blobs = append(d.blobs, b)
	}
	// markers over raw body + decoded strings
	allText := body
	for _, b := range d.blobs {
		allText += " " + string(b.bytes)
	}
	d.hits = scanMarkers(allText)
	d.flat = allText
	return d
}

func scanMarkers(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range markers {
		if m.re.MatchString(s) && !seen[m.name] {
			out = append(out, m.name)
			seen[m.name] = true
		}
	}
	return out
}

func tryB64(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

func printBody(body string, d decoded, detail bool, maxHex int) {
	if body == "" {
		fmt.Println("   (empty body)")
		return
	}
	if d.isJSON {
		pp := d.pretty
		if !detail {
			pp = trunc(pp, 1200)
		}
		fmt.Println("   JSON:")
		fmt.Println(pp)
	} else {
		fmt.Printf("   raw (%dB): %s\n", len(body), trunc(strings.ReplaceAll(body, "\n", "\\n"), 300))
	}
	for i, b := range d.blobs {
		fmt.Printf("   ├ base64 blob[%d] src=%q -> %dB decoded\n", i, b.src, len(b.bytes))
		if len(b.hits) > 0 {
			fmt.Printf("   │  markers: %s\n", strings.Join(b.hits, ", "))
		}
		n := maxHex
		if !detail && n > 96 {
			n = 96
		}
		fmt.Print(hexdump(b.bytes, n, "   │  "))
		if len(b.strings) > 0 {
			ss := b.strings
			if !detail && len(ss) > 12 {
				ss = ss[:12]
			}
			fmt.Printf("   │  strings: %s\n", trunc(strings.Join(ss, " | "), detailLen(detail)))
		}
	}
}

func detailLen(detail bool) int {
	if detail {
		return 4000
	}
	return 400
}

func hexdump(b []byte, max int, prefix string) string {
	if len(b) > max {
		b = b[:max]
	}
	var sb strings.Builder
	for i := 0; i < len(b); i += 16 {
		end := i + 16
		if end > len(b) {
			end = len(b)
		}
		sb.WriteString(prefix)
		sb.WriteString(fmt.Sprintf("%04x  ", i))
		for j := i; j < i+16; j++ {
			if j < end {
				sb.WriteString(fmt.Sprintf("%02x ", b[j]))
			} else {
				sb.WriteString("   ")
			}
			if j == i+7 {
				sb.WriteByte(' ')
			}
		}
		sb.WriteString(" |")
		for j := i; j < end; j++ {
			c := b[j]
			if c >= 0x20 && c < 0x7f {
				sb.WriteByte(c)
			} else {
				sb.WriteByte('.')
			}
		}
		sb.WriteString("|\n")
	}
	return sb.String()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(" + fmt.Sprint(len(s)) + "B)"
}
