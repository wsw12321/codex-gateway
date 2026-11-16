package store

import (
	"strings"
	"testing"
)

func TestEgressServerValidation(t *testing.T) {
	valid := EgressServer{Name: "香港出口", Protocol: "ss", Server: "ss.example.com", Port: 8388, Cipher: "aes-256-gcm"}
	for _, cipher := range []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "2022-blake3-aes-128-gcm"} {
		n := valid
		n.Cipher = cipher
		if err := ValidateEgressServer(n); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"https://ss.example.com", "ss.example.com:8388", "user@ss.example.com", "ss.example.com/path", "ss\n.example.com", "SS.example.com", "localhost", "-ss.example.com", "ss..example.com"} {
		n := valid
		n.Server = host
		if ValidateEgressServer(n) == nil {
			t.Errorf("accepted host %q", host)
		}
	}
	for _, mutate := range []func(*EgressServer){func(n *EgressServer) { n.Port = 0 }, func(n *EgressServer) { n.Port = 65536 }, func(n *EgressServer) { n.Cipher = "rc4-md5" }, func(n *EgressServer) { n.Cipher = "2022-blake3-aes-256-gcm" }, func(n *EgressServer) { n.Name = "bad\nname" }, func(n *EgressServer) { n.Protocol = "" }, func(n *EgressServer) { n.Protocol = "http" }, func(n *EgressServer) { n.Username = "user" }} {
		n := valid
		mutate(&n)
		if ValidateEgressServer(n) == nil {
			t.Error("accepted invalid node")
		}
	}
}

func TestSOCKS5EgressServerValidation(t *testing.T) {
	valid := EgressServer{Name: "SOCKS5 出口", Protocol: "socks5", Server: "203.0.113.7", Port: 1080}
	for _, username := range []string{"", "user", strings.Repeat("u", 255)} {
		n := valid
		n.Username = username
		if err := ValidateEgressServer(n); err != nil {
			t.Fatalf("username %q: %v", username, err)
		}
		if egressNeedsPassword(n) != (username != "") {
			t.Fatalf("username %q password requirement changed", username)
		}
	}
	for _, mutate := range []func(*EgressServer){
		func(n *EgressServer) { n.Cipher = "aes-256-gcm" },
		func(n *EgressServer) { n.Username = strings.Repeat("u", 256) },
		func(n *EgressServer) { n.Username = "us\ner" },
		func(n *EgressServer) { n.Server = "socks5://203.0.113.7" },
		func(n *EgressServer) { n.Protocol = "SOCKS5" },
	} {
		n := valid
		mutate(&n)
		if ValidateEgressServer(n) == nil {
			t.Errorf("accepted invalid SOCKS5 node %+v", n)
		}
	}
}
