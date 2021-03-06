// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package martian

import (
	"bufio"
	"bytes"
	gocontext "context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"time"

	"github.com/google/martian/v3/log"
	"github.com/google/martian/v3/mitm"
	"github.com/google/martian/v3/nosigpipe"
	"github.com/google/martian/v3/proxyutil"
	"github.com/google/martian/v3/trafficshape"
)

type SessionModifier func(*Session) error

var errClose = errors.New("closing connection")
var noop = Noop("martian")

func isCloseable(err error) bool {
	if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
		return true
	}

	switch err {
	case io.EOF, io.ErrClosedPipe, errClose:
		return true
	}

	return false
}

// Proxy is an HTTP proxy with support for TLS MITM and customizable behavior.
type Proxy struct {
	roundTripper http.RoundTripper
	dialContext  func(gocontext.Context, string, string) (net.Conn, error)
	timeout      time.Duration
	mitm         *mitm.Config
	proxyURL     *url.URL

	onTLSClosedConnectionError func(gocontext.Context, string, error)

	reqmod RequestModifier
	resmod ResponseModifier
}

// NewProxy returns a new HTTP proxy.
func NewProxy() *Proxy {
	proxy := &Proxy{
		roundTripper: &http.Transport{
			// TODO(adamtanner): This forces the http.Transport to not upgrade requests
			// to HTTP/2 in Go 1.6+. Remove this once Martian can support HTTP/2.
			TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		timeout: 5 * time.Minute,
		reqmod:  noop,
		resmod:  noop,
	}
	proxy.SetDialContext((&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext)
	return proxy
}

// SetRoundTripper sets the http.RoundTripper of the proxy.
func (p *Proxy) SetRoundTripper(rt http.RoundTripper) {
	p.roundTripper = rt

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		tr.Proxy = http.ProxyURL(p.proxyURL)
		tr.DialContext = p.dialContext
	}
}

// SetDownstreamProxy sets the proxy that receives requests from the upstream
// proxy.
func (p *Proxy) SetDownstreamProxy(proxyURL *url.URL) {
	p.proxyURL = proxyURL

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.Proxy = http.ProxyURL(p.proxyURL)
	}
}

// SetTimeout sets the request timeout of the proxy.
func (p *Proxy) SetTimeout(timeout time.Duration) {
	p.timeout = timeout
}

// SetMITM sets the config to use for MITMing of CONNECT requests.
func (p *Proxy) SetMITM(config *mitm.Config) {
	p.mitm = config
}

// SetDial sets the dial func used to establish a connection.
func (p *Proxy) SetDial(dial func(string, string) (net.Conn, error)) {
	p.SetDialContext(func(ctx gocontext.Context, a, b string) (net.Conn, error) {
		return dial(a, b)
	})
}

// SetDialContext sets the dial func used to establish a connection.
func (p *Proxy) SetDialContext(dialContext func(gocontext.Context, string, string) (net.Conn, error)) {
	p.dialContext = func(ctx gocontext.Context, a, b string) (net.Conn, error) {
		c, e := dialContext(ctx, a, b)
		nosigpipe.IgnoreSIGPIPE(c)
		return c, e
	}

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.DialContext = p.dialContext
	}
}

// Close sets the proxy to the closing state so it stops receiving new connections,
// finishes processing any inflight requests, and closes existing connections without
// reading anymore requests from them.
func (p *Proxy) Close() {
	log.Errorf("fcjr-martian: Close() deprecated, does nothing")
}

// Closing returns whether the proxy is in the closing state.
func (p *Proxy) Closing() bool {
	log.Errorf("fcjr-martian: Closing() deprecated, always returns false")
	return false
}

// SetRequestModifier sets the request modifier.
func (p *Proxy) SetRequestModifier(reqmod RequestModifier) {
	if reqmod == nil {
		reqmod = noop
	}

	p.reqmod = reqmod
}

// SetResponseModifier sets the response modifier.
func (p *Proxy) SetResponseModifier(resmod ResponseModifier) {
	if resmod == nil {
		resmod = noop
	}

	p.resmod = resmod
}

func ctxIsDone(gctx gocontext.Context) bool {
	select {
	case <-gctx.Done():
		return true
	default:
		return false
	}
}

// Serve accepts connections from the listener and handles the requests.
func (p *Proxy) Serve(l net.Listener) error {
	return p.ServeContext(gocontext.Background(), l, nil)
}

// Serve accepts connections from the listener and provides a custom handler to
// handle each connection.
func (p *Proxy) ServeContext(gctx gocontext.Context, l net.Listener, handler func(gocontext.Context, net.Conn)) error {
	defer l.Close()

	if handler == nil {
		handler = p.HandleConn
	}

	connc := make(chan net.Conn)
	errc := make(chan error)

	go func() {
		var delay time.Duration
		for {
			conn, err := l.Accept()
			nosigpipe.IgnoreSIGPIPE(conn)
			if err != nil {
				if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
					if delay == 0 {
						delay = 5 * time.Millisecond
					} else {
						delay *= 2
					}
					if max := time.Second; delay > max {
						delay = max
					}

					log.Debugf("martian: temporary error on accept: %v", err)
					time.Sleep(delay)
					continue
				}

				errc <- err
				return
			}
			delay = 0
			log.Debugf("martian: accepted connection from %s", conn.RemoteAddr())
			connc <- conn
		}
	}()

	for {
		select {
		case <-gctx.Done():
			log.Debugf("martian: closing conn")
			return nil
		case err := <-errc:
			log.Errorf("martian: failed to accept: %v", err)
			return err
		case conn := <-connc:
			go handler(gctx, conn)
		}
	}
}

func (p *Proxy) HandleConn(gctx gocontext.Context, conn net.Conn) {
	defer conn.Close()

	if tconn, ok := conn.(*net.TCPConn); ok {
		tconn.SetKeepAlive(true)
		tconn.SetKeepAlivePeriod(3 * time.Minute)
	}

	if ctxIsDone(gctx) {
		return
	}

	brw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	s, err := newSession(conn, brw)
	if err != nil {
		log.Errorf("martian: failed to create session: %v", err)
		return
	}

	ctx, err := withSession(s)
	if err != nil {
		log.Errorf("martian: failed to create context: %v", err)
		return
	}

	for {
		deadline := time.Now().Add(p.timeout)
		conn.SetDeadline(deadline)

		if err := p.handle(gctx, ctx, conn, brw); isCloseable(err) {
			log.Debugf("martian: closing connection: %v", conn.RemoteAddr())
			return
		}
	}
}

func (p *Proxy) handle(gctx gocontext.Context, ctx *Context, conn net.Conn, brw *bufio.ReadWriter) error {
	log.Debugf("martian: waiting for request: %v", conn.RemoteAddr())

	var req *http.Request
	reqc := make(chan *http.Request, 1)
	errc := make(chan error, 1)
	go func() {
		r, err := http.ReadRequest(brw.Reader)
		if err != nil {
			errc <- err
			return
		}
		reqc <- r
	}()
	select {
	case err := <-errc:
		if isCloseable(err) {
			log.Debugf("martian: connection closed prematurely: %v", err)
		} else {
			if c, ok := conn.(*tls.Conn); ok {
				connectionState := c.ConnectionState()
				serverName := connectionState.ServerName
				if p.onTLSClosedConnectionError != nil {
					p.onTLSClosedConnectionError(gctx, serverName, err)
				}
			}
			log.Errorf("martian: failed to read request: %v", err)
		}

		// TODO: TCPConn.WriteClose() to avoid sending an RST to the client.

		return errClose
	case req = <-reqc:
	case <-gctx.Done():
		return errClose
	}
	defer req.Body.Close()

	session := ctx.Session()
	ctx, err := withSession(session)
	if err != nil {
		log.Errorf("martian: failed to build new context: %v", err)
		return err
	}

	req = req.WithContext(gctx)

	link(req, ctx)
	defer unlink(req)

	if tconn, ok := conn.(*tls.Conn); ok {
		session.MarkSecure()

		cs := tconn.ConnectionState()
		req.TLS = &cs
	}

	req.URL.Scheme = "http"
	if session.IsSecure() {
		log.Debugf("martian: forcing HTTPS inside secure session")
		req.URL.Scheme = "https"
	}

	req.RemoteAddr = conn.RemoteAddr().String()
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}

	if req.Method == "CONNECT" {
		if err := p.reqmod.ModifyRequest(req); err != nil {
			log.Errorf("martian: error modifying CONNECT request: %v", err)
			proxyutil.Warning(req.Header, err)
		}
		if session.Hijacked() {
			log.Infof("martian: connection hijacked by request modifier")
			return nil
		}

		if p.mitm != nil {
			log.Debugf("martian: attempting MITM for connection: %s", req.Host)
			res := proxyutil.NewResponse(200, nil, req)

			if err := p.resmod.ModifyResponse(res); err != nil {
				log.Errorf("martian: error modifying CONNECT response: %v", err)
				proxyutil.Warning(res.Header, err)
			}
			if session.Hijacked() {
				log.Infof("martian: connection hijacked by response modifier")
				return nil
			}

			if err := res.Write(brw); err != nil {
				log.Errorf("martian: got error while writing response back to client: %v", err)
			}
			if err := brw.Flush(); err != nil {
				log.Errorf("martian: got error while flushing response back to client: %v", err)
			}

			log.Debugf("martian: completed MITM for connection: %s", req.Host)

			b := make([]byte, 1)
			if _, err := brw.Read(b); err != nil {
				log.Errorf("martian: error peeking message through CONNECT tunnel to determine type: %v", err)
			}

			// Drain all of the rest of the buffered data.
			buf := make([]byte, brw.Reader.Buffered())
			brw.Read(buf)

			// 22 is the TLS handshake.
			// https://tools.ietf.org/html/rfc5246#section-6.2.1
			if b[0] == 22 {
				// Prepend the previously read data to be read again by
				// http.ReadRequest.
				tlsconn := tls.Server(&peekedConn{conn, io.MultiReader(bytes.NewReader(b), bytes.NewReader(buf), conn)}, p.mitm.TLSForHost(req.Host))

				if err := tlsconn.Handshake(); err != nil {
					p.mitm.HandshakeErrorCallback(req, err)
					return err
				}

				var finalTLSconn net.Conn
				finalTLSconn = tlsconn
				// If the original connection was a traffic shaped connection, wrap the tls
				// connection inside a traffic shaped connection too.
				if ptsconn, ok := conn.(*trafficshape.Conn); ok {
					finalTLSconn = ptsconn.Listener.GetTrafficShapedConn(tlsconn)
				}
				brw.Writer.Reset(finalTLSconn)
				brw.Reader.Reset(finalTLSconn)
				return p.handle(gctx, ctx, finalTLSconn, brw)
			}

			// Prepend the previously read data to be read again by http.ReadRequest.
			brw.Reader.Reset(io.MultiReader(bytes.NewReader(b), bytes.NewReader(buf), conn))
			return p.handle(gctx, ctx, conn, brw)
		}

		log.Debugf("martian: attempting to establish CONNECT tunnel: %s", req.URL.Host)
		res, cconn, cerr := p.connect(req)
		if cerr != nil {
			log.Errorf("martian: failed to CONNECT: %v", err)
			res = proxyutil.NewResponse(502, nil, req)
			proxyutil.Warning(res.Header, cerr)

			if err := p.resmod.ModifyResponse(res); err != nil {
				log.Errorf("martian: error modifying CONNECT response: %v", err)
				proxyutil.Warning(res.Header, err)
			}
			if session.Hijacked() {
				log.Infof("martian: connection hijacked by response modifier")
				return nil
			}

			if err := res.Write(brw); err != nil {
				log.Errorf("martian: got error while writing response back to client: %v", err)
			}
			err := brw.Flush()
			if err != nil {
				log.Errorf("martian: got error while flushing response back to client: %v", err)
			}
			return err
		}
		defer res.Body.Close()
		defer cconn.Close()

		if err := p.resmod.ModifyResponse(res); err != nil {
			log.Errorf("martian: error modifying CONNECT response: %v", err)
			proxyutil.Warning(res.Header, err)
		}
		if session.Hijacked() {
			log.Infof("martian: connection hijacked by response modifier")
			return nil
		}
		res.ContentLength = -1
		if err := res.Write(brw); err != nil {
			log.Errorf("martian: got error while writing response back to client: %v", err)
		}
		if err := brw.Flush(); err != nil {
			log.Errorf("martian: got error while flushing response back to client: %v", err)
		}

		cbw := bufio.NewWriter(cconn)
		cbr := bufio.NewReader(cconn)
		defer cbw.Flush()

		copySync := func(w io.Writer, r io.Reader, donec chan<- bool) {
			if _, err := io.Copy(w, r); err != nil && err != io.EOF {
				log.Errorf("martian: failed to copy CONNECT tunnel: %v", err)
			}

			log.Debugf("martian: CONNECT tunnel finished copying")
			donec <- true
		}

		donec := make(chan bool, 2)
		go copySync(cbw, brw, donec)
		go copySync(brw, cbr, donec)

		log.Debugf("martian: established CONNECT tunnel, proxying traffic")
		<-donec
		<-donec
		log.Debugf("martian: closed CONNECT tunnel")

		return errClose
	}

	if err := p.reqmod.ModifyRequest(req); err != nil {
		log.Errorf("martian: error modifying request: %v", err)
		proxyutil.Warning(req.Header, err)
	}
	if session.Hijacked() {
		log.Infof("martian: connection hijacked by request modifier")
		return nil
	}

	res, err := p.roundTrip(ctx, req)
	if err != nil {
		log.Errorf("martian: failed to round trip: %v", err)
		res = proxyutil.NewResponse(502, nil, req)
		proxyutil.Warning(res.Header, err)
	}
	defer res.Body.Close()

	if err := p.resmod.ModifyResponse(res); err != nil {
		log.Errorf("martian: error modifying response: %v", err)
		proxyutil.Warning(res.Header, err)
	}
	if session.Hijacked() {
		log.Infof("martian: connection hijacked by response modifier")
		return nil
	}

	var closing error
	if req.Close || res.Close || ctxIsDone(gctx) {
		log.Debugf("martian: received close request: %v", req.RemoteAddr)
		res.Close = true
		closing = errClose
	}

	// Check if conn is a traffic shaped connection.
	if ptsconn, ok := conn.(*trafficshape.Conn); ok {
		ptsconn.Context = &trafficshape.Context{}
		// Check if the request URL matches any URLRegex in Shapes. If so, set the connections's Context
		// with the required information, so that the Write() method of the Conn has access to it.
		for urlregex, buckets := range ptsconn.LocalBuckets {
			if match, _ := regexp.MatchString(urlregex, req.URL.String()); match {
				if rangeStart := proxyutil.GetRangeStart(res); rangeStart > -1 {
					dump, err := httputil.DumpResponse(res, false)
					if err != nil {
						return err
					}
					ptsconn.Context = &trafficshape.Context{
						Shaping:            true,
						Buckets:            buckets,
						GlobalBucket:       ptsconn.GlobalBuckets[urlregex],
						URLRegex:           urlregex,
						RangeStart:         rangeStart,
						ByteOffset:         rangeStart,
						HeaderLen:          int64(len(dump)),
						HeaderBytesWritten: 0,
					}
					// Get the next action to perform, if there.
					ptsconn.Context.NextActionInfo = ptsconn.GetNextActionFromByte(rangeStart)
					// Check if response lies in a throttled byte range.
					ptsconn.Context.ThrottleContext = ptsconn.GetCurrentThrottle(rangeStart)
					if ptsconn.Context.ThrottleContext.ThrottleNow {
						ptsconn.Context.Buckets.WriteBucket.SetCapacity(
							ptsconn.Context.ThrottleContext.Bandwidth)
					}
					log.Infof(
						"trafficshape: Request %s with Range Start: %d matches a Shaping request %s. Will enforce Traffic shaping.",
						req.URL, rangeStart, urlregex)
				}
				break
			}
		}
	}

	err = res.Write(brw)
	if err != nil {
		log.Errorf("martian: got error while writing response back to client: %v", err)
		if _, ok := err.(*trafficshape.ErrForceClose); ok {
			closing = errClose
		}
	}
	err = brw.Flush()
	if err != nil {
		log.Errorf("martian: got error while flushing response back to client: %v", err)
		if _, ok := err.(*trafficshape.ErrForceClose); ok {
			closing = errClose
		}
	}
	return closing
}

// A peekedConn subverts the net.Conn.Read implementation, primarily so that
// sniffed bytes can be transparently prepended.
type peekedConn struct {
	net.Conn
	r io.Reader
}

// Read allows control over the embedded net.Conn's read data. By using an
// io.MultiReader one can read from a conn, and then replace what they read, to
// be read again.
func (c *peekedConn) Read(buf []byte) (int, error) { return c.r.Read(buf) }

func (p *Proxy) roundTrip(ctx *Context, req *http.Request) (*http.Response, error) {
	if ctx.SkippingRoundTrip() {
		log.Debugf("martian: skipping round trip")
		return proxyutil.NewResponse(200, nil, req), nil
	}

	return p.roundTripper.RoundTrip(req)
}

func (p *Proxy) connect(req *http.Request) (*http.Response, net.Conn, error) {
	if p.proxyURL != nil {
		log.Debugf("martian: CONNECT with downstream proxy: %s", p.proxyURL.Host)

		conn, err := p.dialContext(req.Context(), "tcp", p.proxyURL.Host)
		if err != nil {
			return nil, nil, err
		}
		pbw := bufio.NewWriter(conn)
		pbr := bufio.NewReader(conn)

		req.Write(pbw)
		pbw.Flush()

		res, err := http.ReadResponse(pbr, req)
		if err != nil {
			return nil, nil, err
		}

		return res, conn, nil
	}

	log.Debugf("martian: CONNECT to host directly: %s", req.URL.Host)

	conn, err := p.dialContext(req.Context(), "tcp", req.URL.Host)
	if err != nil {
		return nil, nil, err
	}

	return proxyutil.NewResponse(200, nil, req), conn, nil
}

func (p *Proxy) SetOnClosedConnectionError(cb func(gocontext.Context, string, error)) {
	p.onTLSClosedConnectionError = cb
}
