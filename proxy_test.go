package dgodriver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestAuthenticatedHTTPSProxy(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")) {
			w.WriteHeader(407)
			return
		}
		_, _ = w.Write([]byte("TLS proxy"))
	}))
	defer upstream.Close()
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxy, err := NewProxyForwarder(ctx, "https://user:pass@"+upstream.Listener.Addr().String(), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	local, _ := url.Parse(proxy.ProxyServer())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(local)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get("http://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "TLS proxy" {
		t.Fatal(string(body), err)
	}
}

func TestAuthenticatedHTTPProxyAndCONNECT(t *testing.T) {
	var mu sync.Mutex
	authenticated := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")) {
			w.WriteHeader(407)
			return
		}
		mu.Lock()
		authenticated++
		mu.Unlock()
		if r.Method == http.MethodConnect {
			remote, err := net.Dial("tcp", r.Host)
			if err != nil {
				t.Error(err)
				w.WriteHeader(502)
				return
			}
			defer remote.Close()
			client, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer client.Close()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\n\r\n")
			_ = rw.Flush()
			go func() { _, _ = io.Copy(remote, rw); remote.Close() }()
			_, _ = io.Copy(client, remote)
			return
		}
		_, _ = w.Write([]byte("authenticated"))
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	forwarder, err := NewProxyForwarder(ctx, "http://user:pass@"+upstream.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()
	local, _ := url.Parse(forwarder.ProxyServer())
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(local)
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	response, err := client.Get("http://example.invalid/test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "authenticated" {
		t.Fatal(string(body))
	}
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials leaked")
		}
		_, _ = w.Write([]byte("tunnel"))
	}))
	defer destination.Close()
	response, err = client.Get(destination.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "tunnel" {
		t.Fatal(string(body))
	}
	mu.Lock()
	defer mu.Unlock()
	if authenticated != 2 {
		t.Fatal(authenticated)
	}
}

func TestAuthenticatedSOCKS5Proxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	errors := make(chan error, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				var header [2]byte
				if _, err := io.ReadFull(conn, header[:]); err != nil {
					errors <- err
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					errors <- err
					return
				}
				_, _ = conn.Write([]byte{5, 2})
				if _, err := io.ReadFull(conn, header[:]); err != nil {
					errors <- err
					return
				}
				user := make([]byte, int(header[1]))
				_, _ = io.ReadFull(conn, user)
				var length [1]byte
				_, _ = io.ReadFull(conn, length[:])
				password := make([]byte, int(length[0]))
				_, _ = io.ReadFull(conn, password)
				if string(user) != "user" || string(password) != "pass" {
					_, _ = conn.Write([]byte{1, 1})
					return
				}
				_, _ = conn.Write([]byte{1, 0})
				var request [4]byte
				if _, err := io.ReadFull(conn, request[:]); err != nil {
					errors <- err
					return
				}
				var host string
				switch request[3] {
				case 1:
					ip := make([]byte, 4)
					_, _ = io.ReadFull(conn, ip)
					host = net.IP(ip).String()
				case 3:
					_, _ = io.ReadFull(conn, length[:])
					name := make([]byte, int(length[0]))
					_, _ = io.ReadFull(conn, name)
					host = string(name)
				case 4:
					ip := make([]byte, 16)
					_, _ = io.ReadFull(conn, ip)
					host = net.IP(ip).String()
				}
				var port [2]byte
				_, _ = io.ReadFull(conn, port[:])
				remote, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))))
				if err != nil {
					errors <- err
					return
				}
				defer remote.Close()
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1})
				go func() { _, _ = io.Copy(remote, conn); remote.Close() }()
				_, _ = io.Copy(conn, remote)
			}()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	forwarder, err := NewProxyForwarder(ctx, "socks5://user:pass@"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()
	proxy, _ := url.Parse(forwarder.ProxyServer())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxy)
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	for _, secure := range []bool{false, true} {
		var destination *httptest.Server
		if secure {
			destination = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("socks")) }))
		} else {
			destination = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("socks")) }))
		}
		response, err := client.Get(destination.URL)
		if err != nil {
			destination.Close()
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		destination.Close()
		if string(body) != "socks" {
			t.Fatal(string(body))
		}
	}
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
}
