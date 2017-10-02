// Package uwsgi provides http.Handler proxying requests to an uWSGI backend.
package uwsgi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Handler is a http.Handler that proxies requests to an uWSGI backend that can
// be connected to using dialFunc.
//
// Usage example:
//
//	network, addr := "unix", "/path/to/uwsgi.socket"
//	var d net.Dialer
//	fn := func(ctx context.Context) (net.Conn, error) {
//		return d.DialContext(ctx, network, addr)
//	}
//	log.Fatal(http.ListenAndServe("localhost:8080", uwsgi.Handler(fn)))
//
// Handler sets some uWSGI-specific variables:
//
//	QUERY_STRING
//	REQUEST_METHOD
//	CONTENT_TYPE
//	CONTENT_LENGTH
//	REQUEST_URI
//	PATH_INFO
//	SERVER_PROTOCOL
//	SERVER_NAME — value from the "Host:" header
//	HTTPS — only set to "on" if request has https scheme or
//		X-Forwarded-Proto: https header
//	SERVER_PORT — set to "443" if request has https scheme or
//		X-Forwarded-Proto: https header
//	REMOTE_ADDR — either address of connected peer, or leftmost value from
//		X-Forwarded-For header if present
//	REMOTE_PORT — port of connected peer, if can be detected
//
// Note the REMOTE_ADDR variable is populated from X-Forwarded-For if present
// — if server is exposed directly to the public network you may want to ensure
// this header is cleared before passing request to this Handler.
type Handler func(context.Context) (net.Conn, error)

func (dial Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logf := logFunc(r)
	if r.Header.Get("Trailer") != "" {
		http.Error(w, "Request trailers are not supported", http.StatusBadRequest)
		return
	}
	type hdr struct {
		name, value string
	}
	headers := []hdr{
		{"QUERY_STRING", r.URL.RawQuery},
		{"REQUEST_METHOD", r.Method},
		{"CONTENT_TYPE", r.Header.Get("Content-Type")},
		{"CONTENT_LENGTH", strconv.FormatInt(r.ContentLength, 10)},
		{"REQUEST_URI", r.RequestURI},
		{"PATH_INFO", r.URL.Path},
		{"SERVER_PROTOCOL", r.Proto},
		{"SERVER_NAME", r.Host},
	}
	if r.URL.Scheme == "https" || r.Header.Get("X-Forwarded-Proto") == "https" {
		headers = append(headers, hdr{"HTTPS", "on"}, hdr{"SERVER_PORT", "443"})
	} else if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, port, err := net.SplitHostPort(addr.String()); err == nil {
			headers = append(headers, hdr{"SERVER_PORT", port})
		}
	} else {
		headers = append(headers, hdr{"SERVER_PORT", "80"})
	}
	var hasRemoteAddr bool
	if s := r.Header.Get("X-Forwarded-For"); s != "" {
		if i := strings.IndexByte(s, ','); i > 0 {
			s = s[:i]
		}
		headers = append(headers, hdr{"REMOTE_ADDR", s})
		hasRemoteAddr = true
	}
	if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if !hasRemoteAddr {
			headers = append(headers, hdr{"REMOTE_ADDR", host})
		}
		headers = append(headers, hdr{"REMOTE_PORT", port})
	}
	for k, v := range r.Header {
		k2 := "HTTP_" + strings.Map(func(r rune) rune {
			if r == '-' {
				return '_'
			}
			return unicode.ToUpper(r)
		}, k)
		h := hdr{k2, strings.Join(v, ", ")}
		if len(h.name) > maxSize || len(h.value) > maxSize {
			http.Error(w, fmt.Sprintf("Header %q is too large\n", k),
				http.StatusRequestHeaderFieldsTooLarge)
			return
		}
		headers = append(headers, h)
	}
	var size int
	for _, h := range headers {
		if len(h.name) > maxSize || len(h.value) > maxSize {
			http.Error(w, http.StatusText(http.StatusRequestHeaderFieldsTooLarge),
				http.StatusRequestHeaderFieldsTooLarge)
			return
		}
		size += len(h.name) + len(h.value) + 4
	}
	if size > maxSize {
		http.Error(w, http.StatusText(http.StatusRequestHeaderFieldsTooLarge),
			http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	var conn net.Conn
	var err error
	var tempDelay time.Duration
	for {
		if conn, err = dial(r.Context()); err == nil {
			break
		}
		if err == context.Canceled {
			panic(http.ErrAbortHandler)
		}
		if ne, ok := err.(net.Error); ok && ne.Temporary() {
			if tempDelay == 0 {
				tempDelay = 5 * time.Millisecond
			} else {
				tempDelay *= 2
			}
			if tempDelay > time.Second {
				logf("uwsgi backend connect: %v", err)
				http.Error(w, http.StatusText(http.StatusGatewayTimeout),
					http.StatusGatewayTimeout)
				return
			}
			select {
			case <-time.After(tempDelay):
				continue
			case <-r.Context().Done():
				panic(http.ErrAbortHandler)
			}
		}
		logf("uwsgi backend connect: %v", err)
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	defer conn.Close()

	uwsgiHeader := make([]byte, 4)
	binary.LittleEndian.PutUint16(uwsgiHeader[1:3], uint16(size))
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.Write(uwsgiHeader)
	for _, hdr := range headers {
		binary.Write(buf, binary.LittleEndian, uint16(len(hdr.name)))
		buf.WriteString(hdr.name)
		binary.Write(buf, binary.LittleEndian, uint16(len(hdr.value)))
		buf.WriteString(hdr.value)
	}
	if _, err := io.Copy(conn, buf); err != nil {
		logf("uwsgi header packet write: %v", err)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	bufPool.Put(buf)
	if _, err := io.Copy(conn, r.Body); err != nil {
		logf("uwsgi body write: %v", err)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), r)
	if err != nil {
		logf("uwsgi response read: %v", err)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	wHeader := w.Header()
	for k, v := range resp.Header {
		wHeader[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func logFunc(r *http.Request) func(format string, v ...interface{}) {
	srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server)
	if ok && srv.ErrorLog != nil {
		return srv.ErrorLog.Printf
	}
	return func(string, ...interface{}) {}
}

const maxSize = 1<<16 - 1 // max uint16 value (standard uwsgi packet payload size)

var bufPool = sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}
