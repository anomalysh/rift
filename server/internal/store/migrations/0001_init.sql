CREATE TABLE tokens (
    id           text        PRIMARY KEY,
    name         text        NOT NULL DEFAULT '',
    token_hash   text        NOT NULL UNIQUE,
    max_tunnels  integer     NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at   timestamptz,
    expires_at   timestamptz
);

CREATE TABLE reservations (
    subdomain  text        PRIMARY KEY,
    token_id   text        NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
    note       text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL
);

CREATE TABLE tunnels (
    id           text        PRIMARY KEY,
    subdomain    text        NOT NULL,
    token_id     text        NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
    protocol     text        NOT NULL DEFAULT 'http',
    local_port   integer     NOT NULL DEFAULT 0,
    node_id      text        NOT NULL DEFAULT '',
    client_addr  text        NOT NULL DEFAULT '',
    connected_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    CONSTRAINT tunnels_subdomain_key UNIQUE (subdomain)
);

CREATE INDEX tunnels_last_seen_at_idx ON tunnels (last_seen_at);
CREATE INDEX tunnels_token_id_idx     ON tunnels (token_id);
CREATE INDEX tunnels_node_id_idx      ON tunnels (node_id);
