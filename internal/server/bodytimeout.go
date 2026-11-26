package server

import (
	"io"
	"net/http"
	"time"
)

// Request-body read time (#172). ReadHeaderTimeout bounds the headers only,
// and http.Server.ReadTimeout cannot be used: it would also cut audio
// streams and long-polls. Instead a request that has a body gets a read
// deadline while the body is read: bodyIdleTimeout without progress, and
// bodyTotalTimeout (bodyUploadTimeout for the analysis results upload) in
// all. The deadline is cleared once the body has been read to its end, so it
// never reaches the response: net/http's background read after the body
// would otherwise hit it and cancel a long stream. A body that failed or was
// not read to the end keeps its deadline, which also bounds net/http's drain
// of the rest after the handler returns (without it, a stalled client would
// hold the connection forever); no background read runs in that case.
var (
	bodyIdleTimeout   = 30 * time.Second
	bodyTotalTimeout  = 2 * time.Minute
	bodyUploadTimeout = 10 * time.Minute
)

// deadlineBody sets the connection's read deadline before each read of the
// body and clears it at the end of the body (io.EOF).
type deadlineBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	end  time.Time
	done bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	if !b.done {
		d := time.Now().Add(bodyIdleTimeout)
		if d.After(b.end) {
			d = b.end
		}
		_ = b.rc.SetReadDeadline(d)
	}
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF && !b.done {
		b.done = true
		_ = b.rc.SetReadDeadline(time.Time{})
	} else if err != nil {
		b.done = true // keep the deadline: it bounds the drain
	}
	return n, err
}

// boundBodyRead applies the deadline to a request with a body; total is the
// most the whole body may take. A request without a body (GET, the relay's
// WebSocket upgrade) is passed on unchanged.
func boundBodyRead(w http.ResponseWriter, r *http.Request, total time.Duration) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return
	}
	r.Body = &deadlineBody{ReadCloser: r.Body, rc: http.NewResponseController(w), end: time.Now().Add(total)}
}
