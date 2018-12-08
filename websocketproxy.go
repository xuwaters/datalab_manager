// Copied from https://github.com/yhat/wsutil

// Copyright (c) 2015, Yhat, Inc.
// All rights reserved.

// Redistribution and use in source and binary forms, with or without modification,
// are permitted provided that the following conditions are met:

//   Redistributions of source code must retain the above copyright notice, this
//   list of conditions and the following disclaimer.

//   Redistributions in binary form must reproduce the above copyright notice, this
//   list of conditions and the following disclaimer in the documentation and/or
//   other materials provided with the distribution.

// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
// ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
// WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
// DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR
// ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
// (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
// LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON
// ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
// SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package main

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ReverseProxy is a WebSocket reverse proxy. It will not work with a regular
// HTTP request, so it is the caller's responsiblity to ensure the incoming
// request is a WebSocket request.
type ReverseProxy struct {
	// Director must be a function which modifies
	// the request into a new request to be sent
	// using Transport. Its response is then copied
	// back to the original client unmodified.
	Director func(*http.Request)

	// Dial specifies the dial function for dialing the proxied
	// server over tcp.
	// If Dial is nil, net.Dial is used.
	Dial func(network, addr string) (net.Conn, error)

	// TLSClientConfig specifies the TLS configuration to use for 'wss'.
	// If nil, the default configuration is used.
	TLSClientConfig *tls.Config

	// ErrorLog specifies an optional logger for errors
	// that occur when attempting to proxy the request.
	// If nil, logging goes to os.Stderr via the log package's
	// standard logger.
	ErrorLog *log.Logger
}

// stolen from net/http/httputil. singleJoiningSlash ensures that the route
// '/a/' joined with '/b' becomes '/a/b'.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

// NewSingleHostReverseProxy returns a new websocket ReverseProxy. The path
// rewrites follow the same rules as the httputil.ReverseProxy. If the target
// url has the path '/foo' and the incoming request '/bar', the request path
// will be updated to '/foo/bar' before forwarding.
// Scheme should specify if 'ws' or 'wss' should be used.
func NewSingleHostReverseProxy(target *url.URL) *ReverseProxy {
	targetQuery := target.RawQuery
	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
    }
    req.Header.Set("Host", req.Host)
	}
	return &ReverseProxy{Director: director}
}

// Function to implement the http.Handler interface.
func (p *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logFunc := log.Printf
	if p.ErrorLog != nil {
		logFunc = p.ErrorLog.Printf
	}

	if !IsWebSocketRequest(r) {
		http.Error(w, "Cannot handle non-WebSocket requests", 500)
		logFunc("Received a request that was not a WebSocket request")
		return
	}

	outreq := new(http.Request)
	// shallow copying
	*outreq = *r
	p.Director(outreq)
	host := outreq.URL.Host

	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := outreq.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		outreq.Header.Set("X-Forwarded-For", clientIP)
	}

	dial := p.Dial
	if dial == nil {
		dial = net.Dial
	}

	// if host does not specify a port, use the default http port
	if !strings.Contains(host, ":") {
		if outreq.URL.Scheme == "wss" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}

	if outreq.URL.Scheme == "wss" {
		var tlsConfig *tls.Config
		if p.TLSClientConfig == nil {
			tlsConfig = &tls.Config{}
		} else {
			tlsConfig = p.TLSClientConfig
		}
		dial = func(network, address string) (net.Conn, error) {
			return tls.Dial("tcp", host, tlsConfig)
		}
	}

	d, err := dial("tcp", host)
	if err != nil {
		http.Error(w, "Error forwarding request.", 500)
		logFunc("Error dialing websocket backend %s: %v", outreq.URL, err)
		return
	}
	// All request generated by the http package implement this interface.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Not a hijacker?", 500)
		return
	}
	// Hijack() tells the http package not to do anything else with the connection.
	// After, it bcomes this functions job to manage it. `nc` is of type *net.Conn.
	nc, _, err := hj.Hijack()
	if err != nil {
		logFunc("Hijack error: %v", err)
		return
	}
	defer nc.Close() // must close the underlying net connection after hijacking
	defer d.Close()

	// write the modified incoming request to the dialed connection
	err = outreq.Write(d)
	if err != nil {
		logFunc("Error copying request to target: %v", err)
		return
	}
	errc := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, err := io.Copy(dst, src)
		errc <- err
	}
	go cp(d, nc)
	go cp(nc, d)
	<-errc
}

// IsWebSocketRequest returns a boolean indicating whether the request has the
// headers of a WebSocket handshake request.
func IsWebSocketRequest(r *http.Request) bool {
	contains := func(key, val string) bool {
		vv := strings.Split(r.Header.Get(key), ",")
		for _, v := range vv {
			if val == strings.ToLower(strings.TrimSpace(v)) {
				return true
			}
		}
		return false
	}
	if !contains("Connection", "upgrade") {
		return false
	}
	if !contains("Upgrade", "websocket") {
		return false
	}
	return true
}
