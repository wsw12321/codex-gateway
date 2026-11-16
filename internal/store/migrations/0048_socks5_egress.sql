-- Account egress nodes may also be SOCKS5 proxies. A SOCKS5 node has no
-- cipher and either no authentication (empty username, NULL password) or an
-- RFC 1929 username with an encrypted password. Existing rows stay Shadowsocks.
-- COALESCE matters: a CHECK that evaluates to NULL passes.
ALTER TABLE egress_servers ADD COLUMN protocol TEXT NOT NULL DEFAULT 'ss';
ALTER TABLE egress_servers ADD COLUMN username TEXT NOT NULL DEFAULT '';
ALTER TABLE egress_servers DROP CONSTRAINT egress_servers_cipher_check;
ALTER TABLE egress_servers DROP CONSTRAINT egress_servers_password_ciphertext_check;
ALTER TABLE egress_servers ALTER COLUMN password_ciphertext DROP NOT NULL;
ALTER TABLE egress_servers ADD CONSTRAINT egress_servers_protocol_check CHECK (
    (protocol = 'ss'
        AND cipher IN ('aes-128-gcm','aes-256-gcm','chacha20-ietf-poly1305','2022-blake3-aes-128-gcm')
        AND username = ''
        AND COALESCE(octet_length(password_ciphertext), 0) > 28)
    OR (protocol = 'socks5'
        AND cipher = ''
        AND octet_length(username) <= 255
        AND ((username = '' AND password_ciphertext IS NULL)
            OR (username <> '' AND COALESCE(octet_length(password_ciphertext), 0) > 28)))
);
