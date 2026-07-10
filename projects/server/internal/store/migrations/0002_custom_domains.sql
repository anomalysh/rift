-- Custom (BYO) domains map a customer-owned hostname to the subdomain whose
-- tunnel serves it (E1). One row per domain; the mapping is upserted whenever
-- an agent reconnects with --domain, so it follows the live tunnel.
CREATE TABLE custom_domains (
    domain     text        PRIMARY KEY,
    subdomain  text        NOT NULL,
    token_id   text        NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX custom_domains_subdomain_idx ON custom_domains (subdomain);
CREATE INDEX custom_domains_token_id_idx  ON custom_domains (token_id);
