-- Almacén de certificados del agente. El agente no tiene disco, pero tampoco
-- puede reemitir en cada arranque (Let's Encrypt permite 5 certificados
-- idénticos por semana), así que guarda acá su material ACME.
--
-- El contenido viaja y se guarda cifrado con una clave derivada del token del
-- agente, del que el servidor solo conoce el hash: estos bytes son opacos
-- para el control plane.
CREATE TABLE agent_storage (
    tunnel_id  bigint NOT NULL REFERENCES tunnel(id) ON DELETE CASCADE,
    key        text NOT NULL,
    value      bytea NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tunnel_id, key)
);
