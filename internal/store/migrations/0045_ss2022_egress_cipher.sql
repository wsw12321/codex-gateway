-- Account egress nodes may use Shadowsocks 2022 with a 16-byte Base64 PSK.
-- Gateway and account-egress validate the key format; the database bounds the
-- method name so an unsupported cipher cannot reach the runtime snapshot.
ALTER TABLE egress_servers DROP CONSTRAINT egress_servers_cipher_check;
ALTER TABLE egress_servers ADD CONSTRAINT egress_servers_cipher_check
    CHECK (cipher IN ('aes-128-gcm','aes-256-gcm','chacha20-ietf-poly1305','2022-blake3-aes-128-gcm'));
