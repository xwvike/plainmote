package web

import (
	"net/http"
	"time"
)

// The server's own timeouts suit a page. A file of some megabytes to or from
// a phone on a poor connection needs longer, and is given time in proportion
// to its size: transferRate is the slowest rate allowed for, and
// transferGrace the time on top.
const (
	transferRate  = 64 << 10
	transferGrace = 30 * time.Second
)

func transferTime(size int64) time.Duration {
	return transferGrace + time.Duration(size/transferRate)*time.Second
}

// allowTransfer gives the response the time size bytes need to go out.
func allowTransfer(w http.ResponseWriter, size int64) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(transferTime(size)))
}

// allowUpload gives the request the time a body of up to limit bytes needs to
// come in. It has to be called before the body is read.
func allowUpload(w http.ResponseWriter, limit int64) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(transferTime(limit)))
}
