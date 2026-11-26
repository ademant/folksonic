package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func shortBodyTimeouts(t *testing.T, idle, total time.Duration) {
	t.Helper()
	oldIdle, oldTotal := bodyIdleTimeout, bodyTotalTimeout
	bodyIdleTimeout, bodyTotalTimeout = idle, total
	t.Cleanup(func() { bodyIdleTimeout, bodyTotalTimeout = oldIdle, oldTotal })
}

// sendSlowly writes the request headers and part of the body, then stalls.
func sendSlowly(t *testing.T, addr, path string, chunks []string, gap time.Duration) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 1000\r\n\r\n", path)
	go func() {
		for _, ch := range chunks {
			time.Sleep(gap)
			if _, err := io.WriteString(c, ch); err != nil {
				return
			}
		}
	}()
	return c
}

// A client that stops sending its body is cut off after the idle timeout,
// and one that trickles it after the total timeout (#172).
func TestSlowRequestBodyIsCutOff(t *testing.T) {
	shortBodyTimeouts(t, 200*time.Millisecond, 600*time.Millisecond)
	readErr := make(chan error, 4)
	srv := httptest.NewServer(limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
	})))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	for name, tc := range map[string]struct {
		chunks []string
		gap    time.Duration
	}{
		"stalled":  {chunks: []string{"a=1"}, gap: 10 * time.Millisecond},
		"trickled": {chunks: strings.Split(strings.Repeat("x", 40), ""), gap: 100 * time.Millisecond},
	} {
		start := time.Now()
		sendSlowly(t, addr, "/rest/createPlaylist", tc.chunks, tc.gap)
		select {
		case err := <-readErr:
			if err == nil {
				t.Errorf("%s: body read did not fail", name)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("%s: cut off after %v", name, d)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: slow body was not cut off", name)
		}
	}
}

// After the body is read, the deadline is gone: a response that takes longer
// than the timeouts (a stream, a long-poll) is not cancelled.
func TestBodyDeadlineDoesNotReachTheResponse(t *testing.T) {
	shortBodyTimeouts(t, 100*time.Millisecond, 200*time.Millisecond)
	srv := httptest.NewServer(limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
			t.Error("request context cancelled after the body was read")
			return
		}
		io.WriteString(w, "done")
	})))
	defer srv.Close()

	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	for i := range 2 { // two requests on one connection: keep-alive still works
		fmt.Fprintf(c, "POST /rest/x HTTP/1.1\r\nHost: x\r\nContent-Length: 3\r\n\r\na=%d", i)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(b) != "done" {
			t.Fatalf("request %d: %d %q", i, resp.StatusCode, b)
		}
	}
}
