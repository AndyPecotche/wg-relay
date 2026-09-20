-- IP pública IPv4 del nodo. NULL = el nodo no participa en las respuestas
-- DNS autoritativas propias (internal/dnsserver), aunque siga ruteando
-- tráfico normalmente: solo lo usa el catch-all de la zona clients.* para
-- armar el set de IPs "edge". Sin backfill: nodos creados antes de esto
-- simplemente no cuentan hasta que se les cargue con `node set-public-ip`.
ALTER TABLE node ADD COLUMN public_ip inet NULL;
