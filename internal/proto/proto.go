// Package proto define los tipos que intercambian el control plane, los nodos
// y los agentes. Es la única fuente de verdad del contrato de la API.
package proto

// Códigos de error estables que los clientes pueden interpretar.
const (
	ErrUnauthorized = "unauthorized"
	ErrLeaseHeld    = "lease_held" // otra instancia del agente tiene el túnel
	ErrSuperseded   = "superseded" // esta instancia perdió el lease
	ErrBadRequest   = "bad_request"
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
	Version string  `json:"version"`
	Peers   []Peer  `json:"peers"`
	Routes  []Route `json:"routes"`
}

type Peer struct {
	PublicKey string `json:"public_key"`
	VPNIP     string `json:"vpn_ip"`
}

// Route asocia un hostname (exacto o "*.dominio") con la IP VPN de un agente.
type Route struct {
	Hostname string `json:"hostname"`
	VPNIP    string `json:"vpn_ip"`
}
