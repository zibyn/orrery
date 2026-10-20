package api

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// captureConn records what relayHTTP writes and answers with a canned response.
type captureConn struct {
	response *bytes.Reader
	written  bytes.Buffer
}

func (c *captureConn) Read(p []byte) (int, error)       { return c.response.Read(p) }
func (c *captureConn) Write(p []byte) (int, error)      { return c.written.Write(p) }
func (c *captureConn) Close() error                     { return nil }
func (c *captureConn) LocalAddr() net.Addr              { return nil }
func (c *captureConn) RemoteAddr() net.Addr             { return nil }
func (c *captureConn) SetDeadline(time.Time) error      { return nil }
func (c *captureConn) SetReadDeadline(time.Time) error  { return nil }
func (c *captureConn) SetWriteDeadline(time.Time) error { return nil }

// A proxy in front of the Control Plane (nginx's connection_upgrade map) sends
// Connection: close for ordinary requests. Forwarding that flag down the
// tunnel makes the Agent close the tunnel as soon as it writes the response,
// racing the Control Plane's read and surfacing as a 503.
func TestRelayHTTPDropsConnectionClose(t *testing.T) {
	conn := &captureConn{response: bytes.NewReader([]byte(
		"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n"))}
	req := httptest.NewRequest(http.MethodGet, "https://cp.example.test/sandboxes/x/files?path=.", nil)
	req.RequestURI = ""
	req.Close = true
	req.Header.Set("Connection", "close")

	relayHTTP(httptest.NewRecorder(), conn, req, "unavailable")

	if bytes.Contains(conn.written.Bytes(), []byte("Connection: close")) {
		t.Fatalf("relay forwarded Connection: close:\n%s", conn.written.String())
	}
}
