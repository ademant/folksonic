package server

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/tsirysndr/tinysonic/internal/logging"
)

// The device registry for remote control (#159). A device publishes a record of
// its own: kind, name and public key material. folksonic does not read the key;
// it is only a rendezvous for devices of the same user, which trust each other
// by their own pairing check. Records are never visible to another user.

const (
	// maxRelayKey bounds the public key material, in characters (base64url).
	maxRelayKey = 1024
	// maxRelayName bounds a device's display name, in characters.
	maxRelayName = 64
)

// relayKinds are the kinds of device a record may describe.
var relayKinds = map[string]bool{"studio": true, "phone": true}

// validRelayKey accepts 1 to maxRelayKey characters of base64url, without padding.
func validRelayKey(k string) bool {
	if k == "" || len(k) > maxRelayKey {
		return false
	}
	for _, c := range k {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// APIRelayDevices lists the caller's own device records.
func (s *State) APIRelayDevices(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rows, err := s.Pool.QueryContext(r.Context(),
		"SELECT device_id, kind, name, public_key, updated_ms FROM relay_devices WHERE user_id = ? ORDER BY device_id", u.ID)
	if err != nil {
		logging.Errorf("relay devices: %v", err)
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []J{}
	for rows.Next() {
		var id, kind, name, key string
		var updated int64
		if err := rows.Scan(&id, &kind, &name, &key, &updated); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, J{"id": id, "kind": kind, "name": name, "publicKey": key, "updatedAt": isoTime(updated)})
	}
	if err := rows.Err(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"devices": out})
}

// APIRelayPutDevice publishes or updates the caller's record for one device.
func (s *State) APIRelayPutDevice(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !validPlayerID(id) {
		writeAPIError(w, http.StatusBadRequest, "device id must be 1 to 64 letters, digits, dots, dashes or underscores")
		return
	}
	var in struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		PublicKey string `json:"publicKey"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if !relayKinds[in.Kind] {
		writeAPIError(w, http.StatusBadRequest, "kind must be studio or phone")
		return
	}
	if !validRelayKey(in.PublicKey) {
		writeAPIError(w, http.StatusBadRequest, "publicKey must be base64url, 1 to 1024 characters")
		return
	}
	name := []rune(in.Name)
	if len(name) > maxRelayName {
		name = name[:maxRelayName]
	}
	_, err := s.Pool.ExecContext(r.Context(),
		"INSERT INTO relay_devices (user_id, device_id, kind, name, public_key, updated_ms) VALUES (?, ?, ?, ?, ?, ?) "+
			"ON CONFLICT(user_id, device_id) DO UPDATE SET kind = excluded.kind, name = excluded.name, "+
			"public_key = excluded.public_key, updated_ms = excluded.updated_ms",
		u.ID, id, in.Kind, string(name), in.PublicKey, time.Now().UnixMilli())
	if err != nil {
		logging.Errorf("relay put %s: %v", id, err)
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"ok": true})
}

// APIRelayDeleteDevice removes one of the caller's device records. A relay
// connection of that device ends at once.
func (s *State) APIRelayDeleteDevice(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !validPlayerID(id) {
		writeAPIError(w, http.StatusBadRequest, "device id is not valid")
		return
	}
	res, err := s.Pool.ExecContext(r.Context(),
		"DELETE FROM relay_devices WHERE user_id = ? AND device_id = ?", u.ID, id)
	if err != nil {
		logging.Errorf("relay delete %s: %v", id, err)
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeAPIError(w, http.StatusNotFound, "no such device on this account")
		return
	}
	s.relay.disconnect(u.ID, id)
	writeJSON(w, http.StatusOK, J{"ok": true})
}

// relayDeviceExists reports whether the user has a record for the device.
func relayDeviceExists(r *http.Request, db *sql.DB, userID int64, id string) (bool, error) {
	var n int
	err := db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM relay_devices WHERE user_id = ? AND device_id = ?", userID, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return n > 0, err
}
