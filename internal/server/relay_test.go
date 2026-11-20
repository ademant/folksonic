package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// relayServer serves the routes behind the same middleware as production, so
// the upgrade goes through the request logger's wrapper (#159).
func relayServer(t *testing.T, s *State) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(requestLogger(securityHeaders(corsMiddleware(limitBody(s.routes())))))
	t.Cleanup(srv.Close)
	return srv
}

// rawWS is a test client: it speaks RFC 6455 on a plain TCP connection.
type rawWS struct {
	c net.Conn
	r *bufio.Reader
}

// dialRelay opens the relay for device with key. It returns the connection, or
// the HTTP status of a refused upgrade.
func dialRelay(t *testing.T, srv *httptest.Server, key, device string) (*rawWS, int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	req := fmt.Sprintf("GET /api/relay/connect?device=%s HTTP/1.1\r\nHost: test\r\nConnection: Upgrade\r\n"+
		"Upgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nX-Api-Key: %s\r\n\r\n",
		device, base64.StdEncoding.EncodeToString(nonce), key)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	tp := textproto.NewReader(r)
	status, err := tp.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	var code int
	_, _ = fmt.Sscanf(status, "HTTP/1.1 %d", &code)
	if code != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(r, 512))
		_ = conn.Close()
		return nil, code, string(body)
	}
	if _, err := tp.ReadMIMEHeader(); err != nil {
		t.Fatal(err)
	}
	return &rawWS{c: conn, r: r}, code, ""
}

// maskedFrame encodes one client message: masked, as RFC 6455 requires.
func maskedFrame(op byte, payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x80 | op)
	n := len(payload)
	switch {
	case n < 126:
		buf.WriteByte(0x80 | byte(n))
	case n <= 0xFFFF:
		buf.WriteByte(0x80 | 126)
		_ = binary.Write(&buf, binary.BigEndian, uint16(n))
	default:
		buf.WriteByte(0x80 | 127)
		_ = binary.Write(&buf, binary.BigEndian, uint64(n))
	}
	mask := []byte{0x12, 0x34, 0x56, 0x78}
	buf.Write(mask)
	for i, b := range payload {
		buf.WriteByte(b ^ mask[i%4])
	}
	return buf.Bytes()
}

func (w *rawWS) send(t *testing.T, op byte, payload []byte) {
	t.Helper()
	if _, err := w.c.Write(maskedFrame(op, payload)); err != nil {
		t.Fatal(err)
	}
}

// read returns the next server message. Server frames are not masked.
func (w *rawWS) read() (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(w.r, h[:]); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.r, ext[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.r, ext[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(w.r, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

// expectData returns the next data message, skipping pings.
func (w *rawWS) expectData(t *testing.T) (byte, []byte) {
	t.Helper()
	for {
		op, payload, err := w.read()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if op == opPing {
			continue
		}
		return op, payload
	}
}

// expectClose waits for a close frame and returns its code.
func (w *rawWS) expectClose(t *testing.T) uint16 {
	t.Helper()
	for {
		op, payload, err := w.read()
		if err != nil {
			t.Fatalf("no close frame: %v", err)
		}
		if op == opClose {
			if len(payload) < 2 {
				return 0
			}
			return binary.BigEndian.Uint16(payload[:2])
		}
	}
}

// relayRecord publishes a device record for the key's user.
func relayRecord(t *testing.T, s *State, key, id, kind string) {
	t.Helper()
	body := fmt.Sprintf(`{"kind":%q,"name":%q,"publicKey":"cHVibGljLWtleS1tYXRlcmlhbA"}`, kind, id)
	if rec := remoteCall(t, s, http.MethodPut, "/api/relay/devices/"+id, key, body); rec.Code != http.StatusOK {
		t.Fatalf("publish %s: %d %s", id, rec.Code, rec.Body.String())
	}
}

// A device's record is visible to its own user only, and the device can remove its record.
func TestRelayRegistryIsPerUser(t *testing.T) {
	s, _, bob := keyFixture(t)
	if _, err := users.Create(context.Background(), s.Pool, "carol", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	carol := newKeyFor(t, s, "carol")
	relayRecord(t, s, bob, "phone-1", "phone")
	relayRecord(t, s, bob, "studio-1", "studio")

	rec := remoteCall(t, s, http.MethodGet, "/api/relay/devices", bob, "")
	if !strings.Contains(rec.Body.String(), "studio-1") || !strings.Contains(rec.Body.String(), "phone-1") {
		t.Fatalf("bob's list: %s", rec.Body.String())
	}
	rec = remoteCall(t, s, http.MethodGet, "/api/relay/devices", carol, "")
	if strings.Contains(rec.Body.String(), "studio-1") {
		t.Fatalf("carol sees bob's devices: %s", rec.Body.String())
	}
	if rec := remoteCall(t, s, http.MethodDelete, "/api/relay/devices/studio-1", carol, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("carol removes bob's device: %d, want 404", rec.Code)
	}
	if rec := remoteCall(t, s, http.MethodDelete, "/api/relay/devices/studio-1", bob, ""); rec.Code != http.StatusOK {
		t.Fatalf("bob removes his device: %d", rec.Code)
	}
	if rec := remoteCall(t, s, http.MethodGet, "/api/relay/devices", bob, ""); strings.Contains(rec.Body.String(), "studio-1") {
		t.Fatalf("removed device still listed: %s", rec.Body.String())
	}
}

// Bad kinds and key material are refused.
func TestRelayRegistryRefusesBadRecords(t *testing.T) {
	s, _, bob := keyFixture(t)
	cases := map[string]string{
		"bad kind":  `{"kind":"tablet","publicKey":"abc"}`,
		"bad key":   `{"kind":"phone","publicKey":"ab+c="}`,
		"empty key": `{"kind":"phone","publicKey":""}`,
		"huge key":  fmt.Sprintf(`{"kind":"phone","publicKey":%q}`, strings.Repeat("a", maxRelayKey+1)),
	}
	for name, body := range cases {
		if rec := remoteCall(t, s, http.MethodPut, "/api/relay/devices/phone-1", bob, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
}

// A frame from one device reaches another device of the same user, with the
// sender's id in front, and the payload unchanged.
func TestRelayForwardsWithinUser(t *testing.T) {
	s, _, key := keyFixture(t)
	relayRecord(t, s, key, "phone-1", "phone")
	relayRecord(t, s, key, "studio-1", "studio")
	srv := relayServer(t, s)
	phone, code, _ := dialRelay(t, srv, key, "phone-1")
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("phone upgrade: %d", code)
	}
	defer phone.c.Close()
	studio, code, _ := dialRelay(t, srv, key, "studio-1")
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("studio upgrade: %d", code)
	}
	defer studio.c.Close()

	payload := []byte("ciphertext-bytes")
	phone.send(t, opBinary, append(append([]byte{byte(len("studio-1"))}, "studio-1"...), payload...))
	op, got := studio.expectData(t)
	want := append(append([]byte{byte(len("phone-1"))}, "phone-1"...), payload...)
	if op != opBinary || !bytes.Equal(got, want) {
		t.Fatalf("studio received op %d %q, want binary %q", op, got, want)
	}
}

// A frame to a device that is not connected is dropped, and the sender is told.
func TestRelayUndeliveredNote(t *testing.T) {
	s, _, key := keyFixture(t)
	relayRecord(t, s, key, "phone-1", "phone")
	relayRecord(t, s, key, "studio-1", "studio")
	srv := relayServer(t, s)
	phone, _, _ := dialRelay(t, srv, key, "phone-1")
	defer phone.c.Close()
	phone.send(t, opBinary, append([]byte{byte(len("studio-1"))}, "studio-1"...))
	op, payload := phone.expectData(t)
	if op != opText || !strings.Contains(string(payload), `"undelivered"`) || !strings.Contains(string(payload), "studio-1") {
		t.Fatalf("note: op %d %q", op, payload)
	}
}

// Another user's device is unreachable: a frame to it is undelivered, and it
// receives nothing.
func TestRelayCrossUserIsolation(t *testing.T) {
	s, _, bob := keyFixture(t)
	if _, err := users.Create(context.Background(), s.Pool, "carol", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	carol := newKeyFor(t, s, "carol")
	relayRecord(t, s, bob, "phone-1", "phone")
	relayRecord(t, s, carol, "studio-9", "studio")
	srv := relayServer(t, s)
	carolStudio, _, _ := dialRelay(t, srv, carol, "studio-9")
	defer carolStudio.c.Close()
	bobPhone, _, _ := dialRelay(t, srv, bob, "phone-1")
	defer bobPhone.c.Close()

	bobPhone.send(t, opBinary, append([]byte{byte(len("studio-9"))}, "studio-9"...))
	if op, payload := bobPhone.expectData(t); op != opText || !strings.Contains(string(payload), "undelivered") {
		t.Fatalf("bob's note: op %d %q", op, payload)
	}
	_ = carolStudio.c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := carolStudio.read(); err == nil {
		t.Fatal("carol's device received bob's frame")
	}
}

// A device with no record cannot open the relay.
func TestRelayNeedsARecord(t *testing.T) {
	s, _, key := keyFixture(t)
	srv := relayServer(t, s)
	if _, code, _ := dialRelay(t, srv, key, "ghost"); code != http.StatusNotFound {
		t.Fatalf("connect without a record: %d, want 404", code)
	}
}

// Text messages are refused: the relay carries binary frames only.
func TestRelayRefusesText(t *testing.T) {
	s, _, key := keyFixture(t)
	relayRecord(t, s, key, "phone-1", "phone")
	srv := relayServer(t, s)
	phone, _, _ := dialRelay(t, srv, key, "phone-1")
	defer phone.c.Close()
	phone.send(t, opText, []byte("hello"))
	if code := phone.expectClose(t); code != closeUnsupported {
		t.Fatalf("close code %d, want %d", code, closeUnsupported)
	}
}

// A message over the size limit ends the connection with 1009.
func TestRelayRefusesOversizeMessage(t *testing.T) {
	s, _, key := keyFixture(t)
	relayRecord(t, s, key, "phone-1", "phone")
	relayRecord(t, s, key, "studio-1", "studio")
	srv := relayServer(t, s)
	phone, _, _ := dialRelay(t, srv, key, "phone-1")
	defer phone.c.Close()
	big := append([]byte{byte(len("studio-1"))}, "studio-1"...)
	big = append(big, make([]byte, relayMaxFrame+1)...)
	phone.send(t, opBinary, big)
	if code := phone.expectClose(t); code != closeTooBig {
		t.Fatalf("close code %d, want %d", code, closeTooBig)
	}
}

// Removing a device's record ends its relay connection at once.
func TestRelayDeleteClosesConnection(t *testing.T) {
	s, _, key := keyFixture(t)
	relayRecord(t, s, key, "phone-1", "phone")
	srv := relayServer(t, s)
	phone, _, _ := dialRelay(t, srv, key, "phone-1")
	defer phone.c.Close()
	if rec := remoteCall(t, s, http.MethodDelete, "/api/relay/devices/phone-1", key, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if code := phone.expectClose(t); code != closeNormal {
		t.Fatalf("close code %d, want %d", code, closeNormal)
	}
}

// A device that sends faster than the rate limit is closed with 1008.
func TestRelayRateLimit(t *testing.T) {
	s, _, key := keyFixture(t)
	relayRecord(t, s, key, "phone-1", "phone")
	srv := relayServer(t, s)
	phone, _, _ := dialRelay(t, srv, key, "phone-1")
	defer phone.c.Close()
	frame := append([]byte{byte(len("phone-1"))}, "phone-1"...)
	for i := 0; i < relayRateBurst*3; i++ {
		phone.send(t, opBinary, frame)
	}
	if code := phone.expectClose(t); code != closePolicy {
		t.Fatalf("close code %d, want %d", code, closePolicy)
	}
}

// The handshake answer is the RFC 6455 example.
func TestWSAcceptKeyMatchesRFC(t *testing.T) {
	if got := wsAcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept key %q", got)
	}
}

// Frames of every length class round-trip through the reader.
func TestWSFrameLengths(t *testing.T) {
	for _, n := range []int{0, 1, 125, 126, 300, 16000, relayMaxFrame} {
		client, server := net.Pipe()
		payload := bytes.Repeat([]byte{0xAB}, n)
		go func(b []byte) {
			_, _ = client.Write(maskedFrame(opBinary, b))
		}(payload)
		w := &wsConn{c: server, r: bufio.NewReader(server)}
		op, got, err := w.readFrame()
		if err != nil || op != opBinary || !bytes.Equal(got, payload) {
			t.Fatalf("length %d: op %d err %v, equal=%v", n, op, err, bytes.Equal(got, payload))
		}
		_ = client.Close()
		_ = server.Close()
	}
}
