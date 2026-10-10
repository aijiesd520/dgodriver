package dgodriver

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProxyForwarder adapts authenticated HTTP/HTTPS/SOCKS5 proxies to a local
// HTTP proxy Chromium can use. Close terminates active CONNECT tunnels too.
type ProxyForwarder struct {
	upstream  *url.URL
	tlsConfig *tls.Config
	listener  net.Listener
	server    *http.Server
	transport *http.Transport
	mu        sync.Mutex
	active    map[net.Conn]struct{}
	closed    bool
	once      sync.Once
}

func NewProxyForwarder(ctx context.Context, upstream string, tlsConfig *tls.Config) (*ProxyForwarder, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("dgodriver: proxy URL requires a host")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("dgodriver: unsupported proxy scheme %q", u.Scheme)
	}
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		if strings.HasPrefix(u.Scheme, "socks") {
			port = "1080"
		}
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	if p, e := strconv.Atoi(u.Port()); e != nil || p <= 0 || p > 65535 {
		return nil, fmt.Errorf("dgodriver: invalid proxy port")
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if len(u.User.Username()) > 255 || len(password) > 255 {
			return nil, fmt.Errorf("dgodriver: proxy credentials exceed 255 bytes")
		}
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &ProxyForwarder{upstream: u, tlsConfig: tlsConfig, listener: listener, active: map[net.Conn]struct{}{}}
	p.transport = http.DefaultTransport.(*http.Transport).Clone()
	if strings.HasPrefix(u.Scheme, "socks") {
		p.transport.Proxy = nil
		p.transport.DialContext = func(c context.Context, network, address string) (net.Conn, error) { return p.openTunnel(c, address) }
	} else {
		p.transport.Proxy = http.ProxyURL(u)
		if tlsConfig != nil {
			p.transport.TLSClientConfig = tlsConfig.Clone()
		}
	}
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 15 * time.Second}
	go func() { _ = p.server.Serve(listener) }()
	return p, nil
}
func (p *ProxyForwarder) ProxyServer() string { return "http://" + p.listener.Addr().String() }
func (p *ProxyForwarder) Close() error {
	var err error
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		for c := range p.active {
			_ = c.Close()
		}
		p.mu.Unlock()
		err = p.server.Close()
		p.transport.CloseIdleConnections()
	})
	return err
}
func (p *ProxyForwarder) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		c.Close()
		return false
	}
	p.active[c] = struct{}{}
	return true
}
func (p *ProxyForwarder) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.active, c)
	p.mu.Unlock()
	_ = c.Close()
}
func stripHopHeaders(header http.Header) {
	for _, v := range header.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, h := range []string{"Proxy-Authorization", "Proxy-Authenticate", "Connection", "Proxy-Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(h)
	}
}
func (p *ProxyForwarder) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		remote, err := p.openTunnel(r.Context(), r.Host)
		if err != nil {
			http.Error(w, "upstream proxy connection failed", http.StatusBadGateway)
			return
		}
		if !p.track(remote) {
			return
		}
		defer p.untrack(remote)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "CONNECT unsupported", 500)
			return
		}
		client, rw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		if !p.track(client) {
			return
		}
		defer p.untrack(client)
		if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err = rw.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(remote, rw)
			if tcp, ok := remote.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			} else {
				_ = remote.Close()
			}
			close(done)
		}()
		_, _ = io.Copy(client, remote)
		_ = remote.Close()
		_ = client.Close()
		<-done
		return
	}
	outgoing := r.Clone(r.Context())
	outgoing.RequestURI = ""
	if outgoing.URL.Scheme == "" {
		outgoing.URL.Scheme = "http"
		outgoing.URL.Host = r.Host
	}
	stripHopHeaders(outgoing.Header)
	response, err := p.transport.RoundTrip(outgoing)
	if err != nil {
		http.Error(w, "upstream proxy request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	stripHopHeaders(response.Header)
	for name, values := range response.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (p *ProxyForwarder) openTunnel(ctx context.Context, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", p.upstream.Host)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	rawConnection := conn
	cancelClose := context.AfterFunc(ctx, func() { _ = rawConnection.Close() })
	defer cancelClose()
	if p.upstream.Scheme == "https" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if p.tlsConfig != nil {
			cfg = p.tlsConfig.Clone()
		}
		if cfg.ServerName == "" {
			cfg.ServerName = p.upstream.Hostname()
		}
		secure := tls.Client(conn, cfg)
		if err = secure.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = secure
	}
	if strings.HasPrefix(p.upstream.Scheme, "socks") {
		err = p.socksConnect(conn, address)
		if err != nil {
			return nil, err
		}
	} else {
		if _, _, err = net.SplitHostPort(address); err != nil {
			return nil, fmt.Errorf("dgodriver: invalid CONNECT address")
		}
		authorization := ""
		if p.upstream.User != nil {
			password, _ := p.upstream.User.Password()
			authorization = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(p.upstream.User.Username()+":"+password)) + "\r\n"
		}
		if _, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", address, address, authorization); err != nil {
			return nil, err
		}
		reader := bufio.NewReader(conn)
		response, e := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
		if e != nil {
			return nil, e
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			return nil, fmt.Errorf("dgodriver: upstream CONNECT returned %d", response.StatusCode)
		}
		conn = &bufferedConn{conn, reader}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	success = true
	return conn, nil
}
func (p *ProxyForwarder) socksConnect(conn net.Conn, address string) error {
	methods := []byte{5, 1, 0}
	if p.upstream.User != nil {
		methods = []byte{5, 2, 0, 2}
	}
	if _, err := conn.Write(methods); err != nil {
		return err
	}
	var response [2]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		return err
	}
	if response[0] != 5 {
		return fmt.Errorf("dgodriver: invalid SOCKS version")
	}
	switch response[1] {
	case 0:
	case 2:
		if p.upstream.User == nil {
			return fmt.Errorf("dgodriver: SOCKS proxy requires credentials")
		}
		user := p.upstream.User.Username()
		password, _ := p.upstream.User.Password()
		auth := append([]byte{1, byte(len(user))}, []byte(user)...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, []byte(password)...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, response[:]); err != nil {
			return err
		}
		if response[0] != 1 || response[1] != 0 {
			return fmt.Errorf("dgodriver: SOCKS authentication failed")
		}
	default:
		return fmt.Errorf("dgodriver: SOCKS rejected authentication")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("dgodriver: invalid SOCKS destination port")
	}
	request := []byte{5, 1, 0}
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		request = append(request, 1)
		request = append(request, ip4...)
	} else if ip != nil {
		request = append(request, 4)
		request = append(request, ip.To16()...)
	} else {
		if len(host) > 255 {
			return fmt.Errorf("dgodriver: SOCKS hostname too long")
		}
		request = append(request, 3, byte(len(host)))
		request = append(request, []byte(host)...)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err = conn.Write(request); err != nil {
		return err
	}
	var reply [4]byte
	if _, err = io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 5 || reply[1] != 0 {
		return fmt.Errorf("dgodriver: SOCKS CONNECT failed: %d", reply[1])
	}
	size := 0
	switch reply[3] {
	case 1:
		size = 4
	case 4:
		size = 16
	case 3:
		var n [1]byte
		if _, err = io.ReadFull(conn, n[:]); err != nil {
			return err
		}
		size = int(n[0])
	default:
		return fmt.Errorf("dgodriver: invalid SOCKS address type")
	}
	_, err = io.CopyN(io.Discard, conn, int64(size+2))
	return err
}
