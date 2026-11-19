package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/wsw/codex-gateway/internal/egress"
)

type supplierRouting struct {
	mode, relay, relayKey, source string
}

func loadSupplierRouting() supplierRouting {
	config := supplierRouting{mode: os.Getenv("EGRESS_MODE"), source: "172.28.70.2"}
	if address, err := netip.ParseAddr(os.Getenv("CODEX_RELAY_IP")); err == nil && address.Is4() {
		config.relay = net.JoinHostPort(address.String(), "3130")
	}
	if config.mode == "" {
		switch {
		case os.Getenv("CODEX_RELAY_IP") != "":
			config.mode = "relay"
		default:
			config.mode = "direct"
		}
	}
	if config.mode == "relay" {
		config.relayKey, _ = readControlKey("/run/secrets/supplier_relay_key")
	}
	return config
}

func runSupplierRelay() error {
	key, err := readControlKey("/run/secrets/supplier_relay_key")
	if err != nil {
		return err
	}
	relay := &runtime{key: key, suppliers: supplierRouting{source: "10.77.0.1"}}
	server := http.Server{Addr: "10.77.0.2:3130", ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 4096,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { relay.serveSupplier(w, r, true) })}
	return server.ListenAndServe()
}

var errSupplierDestination = errors.New("supplier destination refused")

func supplierAddresses(ctx context.Context, host string, lookup func(context.Context, string, string) ([]netip.Addr, error)) ([]netip.Addr, error) {
	addresses, err := lookup(ctx, "ip", host+".")
	if err != nil || len(addresses) == 0 || len(addresses) > 32 {
		return nil, errSupplierDestination
	}
	for _, address := range addresses {
		if !egress.PublicSupplierIP(address) {
			return nil, errSupplierDestination
		}
	}
	return addresses, nil
}

func (r *runtime) serveSupplier(w http.ResponseWriter, req *http.Request, relay bool) {
	source, _, _ := net.SplitHostPort(req.RemoteAddr)
	host, port, _ := net.SplitHostPort(req.Host)
	user, password, _ := proxyCredentials(req.Header)
	account, route, verified := egress.VerifySupplierTicket(r.key, password, host, time.Now())
	if req.Method != http.MethodConnect || source != r.suppliers.source || req.RequestURI != req.Host || port != "443" || user != egress.SupplierProxyUser || !verified || (relay != (route == "relay")) || req.ContentLength != 0 || len(req.TransferEncoding) != 0 {
		http.Error(w, "destination rejected", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	if !relay && route == "global" && r.suppliers.mode == "relay" {
		if r.suppliers.relay == "" || r.suppliers.relayKey == "" {
			http.Error(w, "route unavailable", http.StatusBadGateway)
			return
		}
		ticket := egress.SupplierTicket(r.suppliers.relayKey, account, host, "relay", time.Now())
		upstream, err := supplierParent(ctx, r.suppliers.relay, req.Host, egress.SupplierProxyUser, ticket)
		if err != nil {
			http.Error(w, "route unavailable", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		serveSupplierTunnel(w, upstream)
		return
	}
	parent := ""
	if !relay && route == "global" {
		switch r.suppliers.mode {
		case "direct":
		case "shadowsocks":
			parent = "172.28.50.3:17890"
		default:
			http.Error(w, "route unavailable", http.StatusBadGateway)
			return
		}
	} else if !relay {
		r.mu.Lock()
		bound, found := r.routes[route]
		if !r.status.Ready || !found {
			r.mu.Unlock()
			http.Error(w, "route unavailable", http.StatusBadGateway)
			return
		}
		bound.node.active++
		parent = net.JoinHostPort("127.0.0.1", strconv.Itoa(bound.node.port))
		r.mu.Unlock()
		defer func() { r.mu.Lock(); bound.node.active--; r.mu.Unlock() }()
	}
	addresses, err := supplierAddresses(ctx, host, net.DefaultResolver.LookupNetIP)
	if err == nil {
		for _, address := range addresses {
			target := net.JoinHostPort(address.Unmap().String(), "443")
			var upstream net.Conn
			if parent == "" {
				upstream, err = (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "tcp", target)
			} else {
				upstream, err = supplierParent(ctx, parent, target, "", "")
			}
			if err == nil {
				defer upstream.Close()
				serveSupplierTunnel(w, upstream)
				return
			}
		}
	}
	http.Error(w, "route unavailable", http.StatusBadGateway)
}

func supplierParent(ctx context.Context, parent, target, user, ticket string) (net.Conn, error) {
	connection, err := (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "tcp", parent)
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(deadline)
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Host: target}, Host: target, Header: make(http.Header)}
	if user != "" {
		request.SetBasicAuth(user, ticket)
		request.Header.Set("Proxy-Authorization", request.Header.Get("Authorization"))
		request.Header.Del("Authorization")
	}
	if err = request.Write(connection); err != nil {
		_ = connection.Close()
		return nil, err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, request)
	if err != nil || response.StatusCode != http.StatusOK {
		_ = connection.Close()
		return nil, errSupplierDestination
	}
	_ = connection.SetDeadline(time.Time{})
	return bufferedConn{Conn: connection, reader: reader}, nil
}

func serveSupplierTunnel(w http.ResponseWriter, upstream net.Conn) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "tunnel unavailable", http.StatusInternalServerError)
		return
	}
	client, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err = fmt.Fprint(buffer, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || buffer.Flush() != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, buffer)
		_ = upstream.Close()
		_ = client.Close()
		close(done)
	}()
	_, _ = io.Copy(client, upstream)
	_ = upstream.Close()
	_ = client.Close()
	<-done
}
