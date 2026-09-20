-- Registros TXT de ACME DNS-01 creados a pedido del agente, para
-- certificados wildcard sobre el dominio asignado (F1b).
--
-- id es nuestro identificador opaco, el único que el agente conoce.
-- provider_record_id es el id real en el proveedor DNS configurado
-- (internal/dnsprovider): no se expone al agente, para no acoplar el
-- protocolo entre agente y control plane a un proveedor en particular.
CREATE TABLE dns_challenge (
    id                 text PRIMARY KEY,
    tunnel_id          bigint NOT NULL REFERENCES tunnel(id) ON DELETE CASCADE,
    fqdn               text NOT NULL,
    provider_record_id text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX dns_challenge_tunnel_idx ON dns_challenge(tunnel_id);
