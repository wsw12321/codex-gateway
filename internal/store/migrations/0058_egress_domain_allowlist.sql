-- Host names the Owner allows for member egress servers. A member node
-- otherwise accepts only a public IPv4 literal. An entry is an exact name or a
-- "*." subdomain wildcard and applies to every port.
CREATE TABLE egress_allowed_domains (
    domain TEXT PRIMARY KEY CHECK (
        length(domain) BETWEEN 3 AND 255
        AND domain = lower(domain)
        AND domain !~ '[[:space:]/:@]'
    ),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE egress_allowed_domains IS
    'Owner-managed host names that member egress servers may use instead of a public IPv4 literal.';
