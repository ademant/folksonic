package server

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/logging"
)

// The device relay (#159): one WebSocket per device, over which devices of the
// same user exchange end-to-end encrypted frames. folksonic forwards each frame
// to its recipient and nothing else: it does not read the payload, store it or
// log it. The WebSocket protocol (RFC 6455) is implemented here on the standard
// library, so there is no new dependency.
//
// A binary frame from a device is [1-byte recipient length][recipient id][payload].
// The recipient receives [1-byte sender length][sender id][payload].

const (
	// relayMaxFrame bounds the payload of one message, in bytes.
	relayMaxFrame = 16 * 1024
	// relayRatePerSec and relayRateBurst bound the messages from one device.
	relayRatePerSec = 30
	relayRateBurst  = 60
	// relayMaxConnsPerUser bounds the devices of one user connected at once.
	relayMaxConnsPerUser = 8
	// relayPingEvery is how often the server pings; relayIdle is how long a device
	// may stay silent before the connection is dropped.
	relayPingEvery = 30 * time.Second
	relayIdle      = 60 * time.Second
	relayWriteWait = 10 * time.Second
	// relayMaxWire bounds one WebSocket message off the wire: the payload plus the
	// addressing header and its longest id.
	relayMaxWire = relayMaxFrame + 2 + 64
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes and the close codes used here (RFC 6455 sections 5.2 and 7.4).
const (
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xA

	closeNormal      = 1000
	closeProtocol    = 1002
	closeUnsupported = 1003
	closePolicy      = 1008
	closeTooBig      = 1009
)

var (
	errWSProtocol = errors.New("websocket protocol error")
	errWSTooBig   = errors.New("websocket message too big")
)

// wsConn is one side of an upgraded connection. Writes are serialised, since the
// relay writes to it from other devices' goroutines.
type wsConn struct {
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex
}

// wsAcceptKey is the Sec-WebSocket-Accept value for a client's key.
func wsAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// wsUpgrade checks the handshake and takes over the connection. It writes the
// error response itself and returns false when the request is not a valid upgrade.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, bool) {
	connection := strings.ToLower(r.Header.Get("Connection"))
	key := r.Header.Get("Sec-WebSocket-Key")
	raw, err := base64.StdEncoding.DecodeString(key)
	if !strings.Contains(connection, "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" || err != nil || len(raw) != 16 {
		writeAPIError(w, http.StatusBadRequest, "a WebSocket upgrade request is needed")
		return nil, false
	}
	rc := http.NewResponseController(w)
	conn, brw, err := rc.Hijack()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the connection cannot be upgraded")
		return nil, false
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
	_ = conn.SetWriteDeadline(time.Now().Add(relayWriteWait))
	if _, err := io.WriteString(conn, resp); err != nil {
		_ = conn.Close()
		return nil, false
	}
	return &wsConn{c: conn, r: brw.Reader}, true
}

// readFrame reads one message. Client frames must be masked, and fragmented
// messages are refused, since no client here sends them.
func (w *wsConn) readFrame() (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(w.r, h[:]); err != nil {
		return 0, nil, err
	}
	fin := h[0]&0x80 != 0
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	if !fin || !masked || (op != opText && op != opBinary && op != opClose && op != opPing && op != opPong) {
		return 0, nil, errWSProtocol
	}
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
	if op >= opClose && n > 125 {
		return 0, nil, errWSProtocol
	}
	if n > relayMaxWire {
		return 0, nil, errWSTooBig
	}
	var mask [4]byte
	if _, err := io.ReadFull(w.r, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(w.r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return op, payload, nil
}

// writeFrame sends one unmasked message. Server frames are not masked (RFC 6455 5.1).
func (w *wsConn) writeFrame(op byte, payload []byte) error {
	var hdr [10]byte
	hdr[0] = 0x80 | op
	n := len(payload)
	hl := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xFFFF:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hl = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hl = 10
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(relayWriteWait))
	buf := append(hdr[:hl:hl], payload...)
	_, err := w.c.Write(buf)
	return err
}

// closeWith sends a close frame. The connection stays open until the peer's close
// reply (RFC 6455 section 5.5.1); finish then closes it.
func (w *wsConn) closeWith(code uint16, reason string) {
	body := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(body, code)
	body = append(body, reason...)
	_ = w.writeFrame(opClose, body)
}

// closeSoon sends a close frame and closes the connection after relayWriteWait,
// whether or not the peer replies.
func (w *wsConn) closeSoon(code uint16, reason string) {
	w.closeWith(code, reason)
	time.AfterFunc(relayWriteWait, func() { _ = w.c.Close() })
}

// finish closes a connection that is done. It reads and discards what the peer
// still sends for a moment first: closing with unread data would reset the
// connection and could lose the close frame before the peer reads it.
func (w *wsConn) finish() {
	_ = w.c.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = io.Copy(io.Discard, w.r)
	_ = w.c.Close()
}

// relayConn is one device connected to the relay.
type relayConn struct {
	ws   *wsConn
	user int64
	dev  string
}

// relayHub holds the connected devices by user. The zero value is ready to use.
type relayHub struct {
	mu    sync.Mutex
	conns map[int64]map[string]*relayConn
}

// add registers a connection. A device that connects again replaces its earlier
// connection, which is returned so the caller can close it. ok is false when the
// user already has the maximum number of devices connected.
func (h *relayHub) add(c *relayConn) (replaced *relayConn, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns == nil {
		h.conns = map[int64]map[string]*relayConn{}
	}
	m := h.conns[c.user]
	if m == nil {
		m = map[string]*relayConn{}
		h.conns[c.user] = m
	}
	if prev, exists := m[c.dev]; exists {
		m[c.dev] = c
		return prev, true
	}
	if len(m) >= relayMaxConnsPerUser {
		return nil, false
	}
	m[c.dev] = c
	return nil, true
}

// remove unregisters c, if it is still the registered connection of its device.
func (h *relayHub) remove(c *relayConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.conns[c.user]; m != nil && m[c.dev] == c {
		delete(m, c.dev)
		if len(m) == 0 {
			delete(h.conns, c.user)
		}
	}
}

// get returns the connection of a device of the user, or nil.
func (h *relayHub) get(user int64, dev string) *relayConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[user][dev]
}

// disconnect ends a device's connection, if it has one.
func (h *relayHub) disconnect(user int64, dev string) {
	h.mu.Lock()
	c := h.conns[user][dev]
	if c != nil {
		delete(h.conns[user], dev)
	}
	h.mu.Unlock()
	if c != nil {
		c.ws.closeSoon(closeNormal, "device removed")
	}
}

// APIRelayConnect upgrades the request to the relay for one of the caller's
// devices. The device must have a record. Credentials are headers, never the URL.
func (s *State) APIRelayConnect(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	dev := r.URL.Query().Get("device")
	if !validPlayerID(dev) {
		writeAPIError(w, http.StatusBadRequest, "device must be a valid device id")
		return
	}
	known, err := relayDeviceExists(r, s.Pool, u.ID, dev)
	if err != nil {
		logging.Errorf("relay connect %s: %v", dev, err)
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	if !known {
		writeAPIError(w, http.StatusNotFound, "no such device on this account; publish its record first")
		return
	}
	ws, ok := wsUpgrade(w, r)
	if !ok {
		return
	}
	conn := &relayConn{ws: ws, user: u.ID, dev: dev}
	replaced, ok := s.relay.add(conn)
	if !ok {
		ws.closeSoon(closePolicy, "too many devices connected")
		ws.finish()
		return
	}
	if replaced != nil {
		replaced.ws.closeSoon(closeNormal, "replaced by a new connection")
	}
	defer func() {
		s.relay.remove(conn)
		ws.finish()
	}()
	s.relayLoop(conn)
}

// relayLoop reads a device's messages until it leaves, goes silent, or breaks a
// limit. A ping goes out every relayPingEvery to keep the connection alive.
func (s *State) relayLoop(conn *relayConn) {
	ws := conn.ws
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(relayPingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := ws.writeFrame(opPing, nil); err != nil {
					_ = ws.c.Close()
					return
				}
			}
		}
	}()

	tokens := float64(relayRateBurst)
	last := time.Now()
	for {
		_ = ws.c.SetReadDeadline(time.Now().Add(relayIdle))
		op, payload, err := ws.readFrame()
		if err != nil {
			switch {
			case errors.Is(err, errWSTooBig):
				ws.closeWith(closeTooBig, "message too big")
			case errors.Is(err, errWSProtocol):
				ws.closeWith(closeProtocol, "protocol error")
			}
			return
		}
		switch op {
		case opPing:
			_ = ws.writeFrame(opPong, payload)
		case opPong:
		case opClose:
			ws.closeWith(closeNormal, "")
			return
		case opText:
			ws.closeWith(closeUnsupported, "binary frames only")
			return
		case opBinary:
			now := time.Now()
			tokens = min(float64(relayRateBurst), tokens+now.Sub(last).Seconds()*relayRatePerSec)
			last = now
			tokens--
			if tokens < 0 {
				ws.closeWith(closePolicy, "rate limit")
				return
			}
			if err := s.relayRoute(conn, payload); err != nil {
				ws.closeWith(closeTooBig, "message too big")
				return
			}
		}
	}
}

// relayRoute forwards one frame to the recipient of the same user. It returns an
// error only for an oversize payload, which ends the connection. A frame that
// cannot be delivered is reported to the sender by a control message, and its
// content is dropped. A failed control message means the sender is gone, which
// the read loop finds out itself.
func (s *State) relayRoute(from *relayConn, payload []byte) error {
	if len(payload) < 2 {
		_ = sendRelayNote(from.ws, `{"type":"error","error":"bad_frame"}`)
		return nil
	}
	rl := int(payload[0])
	if rl == 0 || len(payload) < 1+rl {
		_ = sendRelayNote(from.ws, `{"type":"error","error":"bad_frame"}`)
		return nil
	}
	to := string(payload[1 : 1+rl])
	body := payload[1+rl:]
	if !validPlayerID(to) || to == from.dev {
		_ = sendRelayNote(from.ws, `{"type":"error","error":"bad_frame"}`)
		return nil
	}
	if len(body) > relayMaxFrame {
		return errWSTooBig
	}
	dst := s.relay.get(from.user, to)
	if dst == nil {
		_ = sendRelayNote(from.ws, `{"type":"undelivered","to":"`+to+`"}`)
		return nil
	}
	out := make([]byte, 0, 1+len(from.dev)+len(body))
	out = append(out, byte(len(from.dev)))
	out = append(out, from.dev...)
	out = append(out, body...)
	if err := dst.ws.writeFrame(opBinary, out); err != nil {
		_ = sendRelayNote(from.ws, `{"type":"undelivered","to":"`+to+`"}`)
	}
	return nil
}

// sendRelayNote sends a small text control message to one device.
func sendRelayNote(ws *wsConn, note string) error {
	return ws.writeFrame(opText, []byte(note))
}
