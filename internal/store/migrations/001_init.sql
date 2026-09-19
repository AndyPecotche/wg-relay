CREATE TABLE account (
    id          bigserial PRIMARY KEY,
    email       text NOT NULL UNIQUE,
    plan        text NOT NULL DEFAULT 'persistent' CHECK (plan IN ('free', 'persistent')),
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Desplazamiento dentro de 10.64.0.0/10. Nunca se reutiliza una IP.
CREATE SEQUENCE tunnel_ip_seq START 2;

-- Un tunnel = un servidor del cliente = un token.
CREATE TABLE tunnel (
    id          bigserial PRIMARY KEY,
    account_id  bigint NOT NULL REFERENCES account(id) ON DELETE CASCADE,
    subdomain   text NOT NULL UNIQUE,
    vpn_ip      inet NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- UNIQUE(tunnel_id): un token vigente por tunnel. Rotar = reemplazar la fila.
CREATE TABLE token (
    id           text PRIMARY KEY,
    tunnel_id    bigint NOT NULL UNIQUE REFERENCES tunnel(id) ON DELETE CASCADE,
    secret_hash  bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);

-- Qué instancia del agente opera el tunnel ahora. La clave WG es efímera (vive
-- solo en memoria del agente) y se registra con cada lease.
CREATE TABLE agent_lease (
    tunnel_id      bigint PRIMARY KEY REFERENCES tunnel(id) ON DELETE CASCADE,
    instance_id    text NOT NULL,
    wg_pubkey      text NOT NULL UNIQUE,
    agent_version  text NOT NULL DEFAULT '',
    acquired_at    timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL
);

CREATE SEQUENCE node_gw_seq START 1 MAXVALUE 254;

CREATE TABLE node (
    id            bigserial PRIMARY KEY,
    name          text NOT NULL UNIQUE,
    endpoint      text NOT NULL,
    gateway_ip    inet NOT NULL UNIQUE,
    token_id      text NOT NULL UNIQUE,
    token_hash    bytea NOT NULL,
    wg_pubkey     text,
    node_version  text,
    last_seen_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
