package store

import "testing"

func TestEgressServerValidation(t *testing.T) {
	valid := EgressServer{Name: "香港出口", Server: "ss.example.com", Port: 8388, Cipher: "aes-256-gcm"}
	for _, cipher := range []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"} {
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
	for _, mutate := range []func(*EgressServer){func(n *EgressServer) { n.Port = 0 }, func(n *EgressServer) { n.Port = 65536 }, func(n *EgressServer) { n.Cipher = "rc4-md5" }, func(n *EgressServer) { n.Name = "bad\nname" }} {
		n := valid
		mutate(&n)
		if ValidateEgressServer(n) == nil {
			t.Error("accepted invalid node")
		}
	}
}
