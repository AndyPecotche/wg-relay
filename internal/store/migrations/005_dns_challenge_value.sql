-- El valor del TXT ahora se guarda siempre en la base, no solo en el
-- proveedor DNS externo: es lo que permite que internal/dnsserver sirva los
-- desafíos ACME sin depender de nadie más. provider_record_id pasa a ser
-- opcional: solo se completa si además hay un dnsprovider.Provider
-- configurado (uso híbrido, ortogonal al DNS propio).
ALTER TABLE dns_challenge ADD COLUMN value text NOT NULL DEFAULT '';
ALTER TABLE dns_challenge ALTER COLUMN provider_record_id DROP NOT NULL;
