package api

import "testing"

// validACMEDNSName es la única puerta que evita que un tunnel escriba un TXT
// fuera de su propia zona: cubrirla bien importa más que el resto del paquete.
func TestValidACMEDNSName(t *testing.T) {
	domain := "dsk7yrh.clients.wg-relay.andy.net.ar"
	valid := []string{
		"_acme-challenge." + domain,
		"_acme-challenge.dev." + domain, // comodín más profundo: *.dev.<sub>.<base>
	}
	for _, fqdn := range valid {
		if !validACMEDNSName(fqdn, domain) {
			t.Errorf("validACMEDNSName(%q) = false, quería true", fqdn)
		}
	}

	invalid := []string{
		"_acme-challenge.otrotunnel.clients.wg-relay.andy.net.ar", // otro tunnel
		"_acme-challenge.evil.com",                                // otro dominio
		"_acme-challenge.wg-relay.andy.net.ar",                    // el dominio padre, no el del tunnel
		domain,                                                    // sin el prefijo _acme-challenge
		"_acme-challenge2." + domain,                              // prefijo parecido pero no exacto
		"_acme-challenge." + domain + "x",                         // sufijo con el dominio pegado (no separado por ".")
		"",
	}
	for _, fqdn := range invalid {
		if validACMEDNSName(fqdn, domain) {
			t.Errorf("validACMEDNSName(%q) = true, quería false", fqdn)
		}
	}
}
