package wttransport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/pristupaanastasia/turnvpn/internal/wtsignal"
)

// wtStream is the subset of *webtransport.Stream that streamRWC needs: a
// read/write/close stream whose read side must be cancelled explicitly (a QUIC
// stream's Close only closes the write side).
type wtStream interface {
	io.ReadWriteCloser
	CancelRead(webtransport.StreamErrorCode)
}

// streamRWC adapts a WebTransport stream to an io.ReadWriteCloser whose Close also
// cancels the read side, so a wtsignal read loop blocked in Read is unblocked by
// Close instead of hanging (the carry-over from the wtsignal review).
type streamRWC struct {
	s wtStream
}

func (w streamRWC) Read(p []byte) (int, error)  { return w.s.Read(p) }
func (w streamRWC) Write(p []byte) (int, error) { return w.s.Write(p) }

func (w streamRWC) Close() error {
	w.s.CancelRead(0) // unblock a pending Read; harmless if already closed
	return w.s.Close()
}

// Session is a dialed WebTransport signaling session: an embedded
// *wtsignal.Conn (use Messages and Send) plus the WebTransport resources it owns.
// Close tears all of them down.
type Session struct {
	*wtsignal.Conn
	wtSession *webtransport.Session
	transport *webtransport.Transport
}

// Close closes the signaling conn (which cancels the stream read and closes the
// stream), then the WebTransport session and transport. It joins any errors.
func (s *Session) Close() error {
	connErr := s.Conn.Close()
	sessErr := s.wtSession.CloseWithError(0, "")
	trErr := s.transport.Close()
	return errors.Join(connErr, sessErr, trErr)
}

// DialOptions tunes the WebTransport dial.
type DialOptions struct {
	// TLSClientConfig is used for the QUIC/TLS handshake; nil uses a default.
	// The transport sets the HTTP/3 ALPN itself.
	TLSClientConfig *tls.Config
	// Header carries extra request headers for the CONNECT (e.g. a user agent).
	Header http.Header
}

// Dial opens a WebTransport session to signalingURL, opens the bidirectional
// signaling stream, and wraps it as a message-oriented Session using the
// compression declared in the URL (deflate-raw for the WebTransport leg). Build
// signalingURL with BuildSignalingURL. The caller must Close the returned Session.
func Dial(ctx context.Context, signalingURL string, opts DialOptions) (*Session, error) {
	tr := &webtransport.Transport{TLSClientConfig: opts.TLSClientConfig}

	_, wtSess, err := tr.Dial(ctx, signalingURL, opts.Header)
	if err != nil {
		tr.Close()
		return nil, fmt.Errorf("wttransport: dial %s: %w", signalingURL, err)
	}

	stream, err := wtSess.OpenStreamSync(ctx)
	if err != nil {
		wtSess.CloseWithError(0, "")
		tr.Close()
		return nil, fmt.Errorf("wttransport: open stream: %w", err)
	}

	conn := wtsignal.New(streamRWC{stream}, wtsignal.Options{
		Compression: CompressionFromURL(signalingURL),
	})
	return &Session{Conn: conn, wtSession: wtSess, transport: tr}, nil
}
