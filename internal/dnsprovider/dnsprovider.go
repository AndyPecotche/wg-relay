// Package dnsprovider abstrae la gestión de DNS que hace el control plane
// por cuenta propia: el CNAME por tunnel (DESIGN.md §4.2) y el TXT efímero
// del DNS-01 delegado sobre el dominio asignado (§6.2.1). No tiene nada que
// ver con el DNS de un dominio propio del cliente (§6.5, F2): ese lo resuelve
// el agente, hablando directo con el proveedor del cliente.
//
// Cloudflare es la implementación por defecto (internal/cloudflare), sin
// dependencias extra. Para cualquier otro proveedor (Route53, DigitalOcean,
// OVH, un DNS propio, ...) se usa Webhook: en vez de vendorizar el SDK de
// cada nube en el binario, el operador corre un servicio propio, del tamaño
// que quiera, que traduzca estas tres operaciones a lo que su proveedor
// necesite.
package dnsprovider

import "context"

// Provider es lo mínimo que el control plane necesita de un DNS.
type Provider interface {
	// EnsureCNAME crea o corrige name -> target.
	EnsureCNAME(ctx context.Context, name, target string) error
	// CreateTXT agrega un TXT y devuelve un identificador opaco, propio de
	// cada implementación, para poder borrarlo después con DeleteRecord.
	CreateTXT(ctx context.Context, name, value string) (id string, err error)
	// DeleteRecord borra el registro creado por CreateTXT. Si ya no existe,
	// no es error: el estado final deseado ya está logrado.
	DeleteRecord(ctx context.Context, id string) error
}
