// Package proto define los tipos que intercambian el control plane, los nodos
// y los agentes. Es la única fuente de verdad del contrato de la API.
package proto

// Códigos de error estables que los clientes pueden interpretar.
const (
	ErrUnauthorized = "unauthorized"
	ErrLeaseHeld    = "lease_held" // otra instancia del agente tiene el túnel
	ErrSuperseded   = "superseded" // esta instancia perdió el lease
	ErrBadRequest   = "bad_request"
	ErrNotFound     = "not_found"
	ErrRateLimited  = "rate_limited"
	ErrInternal     = "internal"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Node es un relay tal como lo ve un agente.
type Node struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`   // host:puerto UDP de WireGuard
	PublicKey string `json:"public_key"` // base64
	GatewayIP string `json:"gateway_ip"` // IP del nodo dentro de la VPN
}

type RegisterRequest struct {
	InstanceID  string `json:"instance_id"`
	WGPublicKey string `json:"wg_public_key"`
	Version     string `json:"version"`
}

type RegisterResponse struct {
	TunnelID         int64  `json:"tunnel_id"`
	VPNIP            string `json:"vpn_ip"`
	Domain           string `json:"domain"`
	Nodes            []Node `json:"nodes"`
	LeaseTTLSeconds  int    `json:"lease_ttl_seconds"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
}

type HeartbeatRequest struct {
	InstanceID string `json:"instance_id"`
}

type HeartbeatResponse struct {
	Nodes []Node `json:"nodes"`
}

type ReleaseRequest struct {
	InstanceID string `json:"instance_id"`
}

type NodeHelloRequest struct {
	WGPublicKey string `json:"wg_public_key"`
	Version     string `json:"version"`
}

type NodeHelloResponse struct {
	Name      string `json:"name"`
	GatewayIP string `json:"gateway_ip"`
}

// NodeConfig es la foto completa que un nodo necesita para operar.
// Version es un hash del contenido: cambia si y solo si cambia la foto.
type NodeConfig struct {
	Version string   `json:"version"`
	Peers   []Peer   `json:"peers"`
	Routes  []Route  `json:"routes"`
	DNS     *DNSZone `json:"dns,omitempty"` // nil si el DNS propio no está habilitado
}

// DNSZone es la zona clients.* que el nodo puede servir como DNS
// autoritativo propio (internal/dnsserver), si además bindea
// WGRELAY_DNS_LISTEN. Ver DESIGN.md.
type DNSZone struct {
	Zone       string   `json:"zone"`     // ej. "clients.wg-relay.andy.net.ar"
	NSNames    []string `json:"ns_names"` // FQDNs de nameserver, estático, admin
	SOAEmail   string   `json:"soa_email"`
	Edge       []string `json:"edge"`       // IPs públicas de nodos activos
	Challenges []DNSTXT `json:"challenges"` // TXT vigentes de ACME DNS-01
}

type DNSTXT struct {
	FQDN  string `json:"fqdn"`
	Value string `json:"value"`
}

type Peer struct {
	PublicKey string `json:"public_key"`
	VPNIP     string `json:"vpn_ip"`
}

// StorageItem describe una clave del almacén del agente. El servidor guarda
// los valores cifrados por el agente, así que solo conoce el tamaño y la fecha.
type StorageItem struct {
	Key       string `json:"key"`
	Size      int64  `json:"size"`
	UpdatedAt string `json:"updated_at"`
}

type StorageList struct {
	Items []StorageItem `json:"items"`
}

type ACMEDNSCreateRequest struct {
	FQDN  string `json:"fqdn"`
	Value string `json:"value"`
}

type ACMEDNSCreateResponse struct {
	ID string `json:"id"`
}

// Route asocia un hostname (exacto o "*.dominio") con la IP VPN de un agente.
type Route struct {
	Hostname string `json:"hostname"`
	VPNIP    string `json:"vpn_ip"`
}
