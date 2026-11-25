ALTER TABLE egress_servers
    ADD COLUMN tls_server_name TEXT NOT NULL DEFAULT '',
    DROP CONSTRAINT egress_servers_protocol_check,
    DROP CONSTRAINT egress_servers_server_check,
    DROP CONSTRAINT egress_servers_port_check;

ALTER TABLE egress_servers ADD CHECK (
    (protocol IN ('tailscale','zerotier','easytier') AND server = '' AND port = 0)
    OR (protocol NOT IN ('tailscale','zerotier','easytier') AND length(server) BETWEEN 1 AND 253 AND port BETWEEN 1 AND 65535)
), ADD CHECK (tls_server_name = '' OR (protocol = 'https' AND length(tls_server_name) <= 253)),
   ADD CHECK (protocol NOT IN ('http','https') OR NOT udp),
   ADD CONSTRAINT egress_servers_protocol_check CHECK (
    (protocol = 'ss'
        AND cipher IN ('aes-128-gcm','aes-256-gcm','chacha20-ietf-poly1305','2022-blake3-aes-128-gcm')
        AND username = ''
        AND COALESCE(octet_length(password_ciphertext), 0) > 28)
    OR (protocol IN ('socks5','http','https')
        AND cipher = ''
        AND octet_length(username) <= 255
        AND ((username = '' AND password_ciphertext IS NULL)
            OR (username <> '' AND COALESCE(octet_length(password_ciphertext), 0) > 28)))
    OR (protocol IN ('ssr','vmess','vless','snell','trojan','hysteria','hysteria2','wireguard','tuic',
                     'shadowquic','gost-relay','ssh','mieru','anytls','sudoku','masque','trusttunnel',
                     'openvpn','tailscale','zerotier','easytier')
        AND cipher = '' AND username = '' AND COALESCE(octet_length(password_ciphertext),0) > 28)
);
