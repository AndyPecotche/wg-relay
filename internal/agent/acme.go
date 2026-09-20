package agent

import (
	"crypto/x509"
	"fmt"
	"os"
)

// loadTrustedRoots lee un PEM de raíces confiables para hablar con la CA.
// Vacío es el caso normal (se usan las raíces del sistema); solo hace falta
// completarlo con una CA privada (step-ca, Pebble, PKI interna).
func loadTrustedRoots(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("acme.trusted_roots: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("acme.trusted_roots: %s no contiene certificados", path)
	}
	return roots, nil
}
