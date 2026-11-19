package kernelexit

import (
	"encoding/base64"
	"encoding/hex"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fakeWG records Set calls and serves a fixed current peer set.
type fakeWG struct {
	current map[string]bool
	sets    [][]string
	failSet bool
}

func (f *fakeWG) Dump(string) (map[string]bool, error) { return f.current, nil }
func (f *fakeWG) Set(_ string, args ...string) error {
	if f.failSet {
		return errFake
	}
	f.sets = append(f.sets, args)
	return nil
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "fake set failure" }

func hexKey(n byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = n
	}
	return hex.EncodeToString(b)
}
func b64Key(n byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = n
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestReconcileAddsAndRemoves(t *testing.T) {
	// Current wg0 has peers 0x01 (keep) and 0x09 (stale → remove).
	f := &fakeWG{current: map[string]bool{b64Key(1): true, b64Key(9): true}}
	desired := []Peer{
		{PubKeyHex: hexKey(1), AllowedIP: "10.6.0.2/32"}, // already present
		{PubKeyHex: hexKey(2), AllowedIP: "10.6.0.3/32"}, // new
	}
	if err := Reconcile(f, "wg0", desired); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got []string
	for _, s := range f.sets {
		got = append(got, strings.Join(s, " "))
	}
	sort.Strings(got)
	want := []string{
		"peer " + b64Key(1) + " allowed-ips 10.6.0.2/32",
		"peer " + b64Key(2) + " allowed-ips 10.6.0.3/32",
		"peer " + b64Key(9) + " remove",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("set calls:\n got %v\nwant %v", got, want)
	}
}

func TestReconcileBadHexKey(t *testing.T) {
	f := &fakeWG{current: map[string]bool{}}
	if err := Reconcile(f, "wg0", []Peer{{PubKeyHex: "nothex", AllowedIP: "10.6.0.2/32"}}); err == nil {
		t.Fatal("want error for bad hex key")
	}
	if err := Reconcile(f, "wg0", []Peer{{PubKeyHex: "aabb", AllowedIP: "10.6.0.2/32"}}); err == nil {
		t.Fatal("want error for short key")
	}
}

func TestReconcileSetFailure(t *testing.T) {
	f := &fakeWG{current: map[string]bool{}, failSet: true}
	if err := Reconcile(f, "wg0", []Peer{{PubKeyHex: hexKey(1), AllowedIP: "10.6.0.2/32"}}); err == nil {
		t.Fatal("want error when wg set fails")
	}
}

func TestParseDump(t *testing.T) {
	// interface line, then two peers.
	dump := "PRIVKEY\tPUBKEY\t51820\toff\n" +
		b64Key(1) + "\t(none)\t1.2.3.4:1\t10.6.0.2/32\t0\t0\t0\toff\n" +
		b64Key(2) + "\t(none)\t(none)\t10.6.0.3/32\t0\t0\t0\toff\n"
	got := parseDump([]byte(dump))
	want := map[string]bool{b64Key(1): true, b64Key(2): true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDump: got %v want %v", got, want)
	}
}

func TestHexToWGKey(t *testing.T) {
	got, err := hexToWGKey(hexKey(2))
	if err != nil {
		t.Fatalf("hexToWGKey: %v", err)
	}
	if got != b64Key(2) {
		t.Fatalf("hexToWGKey: got %s want %s", got, b64Key(2))
	}
}
