-- Owner-maintained community contact and manual recharge instructions for the
-- login, overview and billing pages. Payments are never verified or credited
-- here: the Owner still records each confirmed payment through billing.
CREATE TABLE site_settings (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    community_enabled BOOLEAN NOT NULL DEFAULT false,
    community_title TEXT NOT NULL DEFAULT '' CHECK (length(community_title) <= 40),
    community_qq_group TEXT NOT NULL DEFAULT '' CHECK (community_qq_group ~ '^([1-9][0-9]{4,11})?$'),
    community_join_url TEXT NOT NULL DEFAULT '' CHECK (length(community_join_url) <= 500 AND (community_join_url = '' OR community_join_url LIKE 'https://%')),
    community_note TEXT NOT NULL DEFAULT '' CHECK (length(community_note) <= 500),
    recharge_enabled BOOLEAN NOT NULL DEFAULT false,
    recharge_contact TEXT NOT NULL DEFAULT '' CHECK (length(recharge_contact) <= 80),
    recharge_note TEXT NOT NULL DEFAULT '' CHECK (length(recharge_note) <= 1000),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO site_settings(singleton) VALUES (true);

-- Images are re-encoded by Gateway before storage, dropping EXIF and any
-- trailing bytes. Only fixed slots exist; SVG is never accepted.
CREATE TABLE site_images (
    slot TEXT PRIMARY KEY CHECK (slot IN ('community_qr','recharge_wechat','recharge_alipay')),
    content_type TEXT NOT NULL CHECK (content_type IN ('image/png','image/jpeg')),
    data BYTEA NOT NULL CHECK (octet_length(data) BETWEEN 1 AND 8388608),
    sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    width INTEGER NOT NULL CHECK (width BETWEEN 1 AND 4096),
    height INTEGER NOT NULL CHECK (height BETWEEN 1 AND 4096),
    updated_at TIMESTAMPTZ NOT NULL
);
