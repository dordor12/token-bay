package ccproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePeerConn is a controllable stand-in for PeerConn.
type fakePeerConn struct {
	sendErr     error
	sentBody    []byte
	receiveOK   bool
	receiveMsg  string
	receiveBody string
	receiveErr  error
	closed      bool
}

func (f *fakePeerConn) Send(body []byte) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sentBody = append([]byte(nil), body...)
	return nil
}

func (f *fakePeerConn) Receive(ctx context.Context) (bool, string, io.Reader, error) {
	if f.receiveErr != nil {
		return false, "", nil, f.receiveErr
	}
	if !f.receiveOK {
		return false, f.receiveMsg, nil, nil
	}
	return true, "", strings.NewReader(f.receiveBody), nil
}

func (f *fakePeerConn) Close() error { f.closed = true; return nil }

// fakeDialer always returns the configured PeerConn or err.
type fakeDialer struct {
	conn *fakePeerConn
	err  error
}

func (d *fakeDialer) Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.conn, nil
}

func TestNetworkRouter_HappyPath_StreamsSSEBytes(t *testing.T) {
	canned := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	conn := &fakePeerConn{receiveOK: true, receiveBody: canned}
	router := &NetworkRouter{Dialer: &fakeDialer{conn: conn}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))
	rec := httptest.NewRecorder()

	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, canned, rec.Body.String())
	assert.Equal(t, []byte(`{"model":"claude-sonnet-4-6"}`), conn.sentBody)
	assert.True(t, conn.closed)
}

func TestNetworkRouter_NilMeta_Returns502(t *testing.T) {
	called := false
	dialer := &fakeDialer{}
	router := &NetworkRouter{Dialer: &dialerSpy{inner: dialer, called: &called}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, nil)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), `"network_unavailable"`)
	assert.False(t, called, "dialer must not be called when meta is nil")
}

type dialerSpy struct {
	inner  PeerDialer
	called *bool
}

func (s *dialerSpy) Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error) {
	*s.called = true
	return s.inner.Dial(ctx, meta)
}

func TestNetworkRouter_DialError_Returns502(t *testing.T) {
	router := &NetworkRouter{Dialer: &fakeDialer{err: errors.New("dial boom")}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), `"network_unavailable"`)
}

func TestNetworkRouter_SendError_Returns502_AndCloses(t *testing.T) {
	conn := &fakePeerConn{sendErr: errors.New("send boom")}
	router := &NetworkRouter{Dialer: &fakeDialer{conn: conn}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.True(t, conn.closed)
}

func TestNetworkRouter_StatusError_Returns502_WithSeederMessage(t *testing.T) {
	conn := &fakePeerConn{receiveOK: false, receiveMsg: "seeder rate limited"}
	router := &NetworkRouter{Dialer: &fakeDialer{conn: conn}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "seeder rate limited")
	assert.True(t, conn.closed)
}

func TestNetworkRouter_ReceiveError_Returns502(t *testing.T) {
	conn := &fakePeerConn{receiveErr: errors.New("recv boom")}
	router := &NetworkRouter{Dialer: &fakeDialer{conn: conn}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.True(t, conn.closed)
}

// errBody is an io.ReadCloser whose Read always errors. Verifies the
// router rejects unreadable request bodies before dialing.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("read boom") }
func (errBody) Close() error             { return nil }

func TestNetworkRouter_BodyReadError_Returns400(t *testing.T) {
	called := false
	dialer := &dialerSpy{inner: &fakeDialer{}, called: &called}
	router := &NetworkRouter{Dialer: dialer}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", errBody{})
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, called)
}

// Existing PassThroughRouter test stays.
func TestPassThroughRouter_ForwardsRequestAndResponse(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"method":"`+r.Method+`","path":"`+r.URL.Path+`"}`)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	router := &PassThroughRouter{UpstreamURL: u}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer secret-token-xyz")
	rec := httptest.NewRecorder()

	router.Route(rec, req, nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"path":"/v1/messages"`)
	assert.Equal(t, "Bearer secret-token-xyz", gotAuth)
}

func TestNewPassThroughRouter_DefaultUpstreamIsAnthropic(t *testing.T) {
	r := NewPassThroughRouter()
	require.NotNil(t, r.UpstreamURL)
	assert.Equal(t, "https", r.UpstreamURL.Scheme)
	assert.Equal(t, "api.anthropic.com", r.UpstreamURL.Host)
}

// Compile-time interface checks.
var (
	_ RequestRouter = (*PassThroughRouter)(nil)
	_ RequestRouter = (*NetworkRouter)(nil)
)
