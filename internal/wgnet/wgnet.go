// Package wgnet levanta una interfaz WireGuard íntegramente en espacio de
// usuario (wireguard-go + la pila TCP/IP de gVisor). No crea interfaces en el
// kernel: no requiere root, NET_ADMIN, /dev/net/tun ni módulos del kernel. Las
// conexiones a través del túnel se abren y aceptan desde el propio proceso.
package wgnet

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

type Key [32]byte

func GenerateKey() Key {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		panic(err)
	}
	return k.clamp()
}

// KeyFromSeed convierte 32 bytes arbitrarios en una clave privada válida.
func KeyFromSeed(seed [32]byte) Key { return Key(seed).clamp() }

func (k Key) clamp() Key {
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return k
}

func (k Key) Public() Key {
	pub, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		panic(err)
	}
	return Key(pub)
}

func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }
func (k Key) hex() string    { return hex.EncodeToString(k[:]) }

func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return Key{}, fmt.Errorf("clave WireGuard inválida")
	}
	return Key(b), nil
}

type Config struct {
	PrivateKey Key
	Address    netip.Addr
	ListenPort int // 0 = puerto aleatorio (agentes)
	MTU        int // 0 = 1420
	Logger     *slog.Logger
}

type Peer struct {
	PublicKey Key
	AllowedIP netip.Prefix
	Endpoint  netip.AddrPort // vacío = el peer inicia (nodos)
	Keepalive int            // segundos, 0 = desactivado
}

type Net struct {
	addr  netip.Addr
	dev   *device.Device
	tnet  *netstack.Net
	mu    sync.Mutex
	peers map[Key]Peer
}

func New(cfg Config) (*Net, error) {
	mtu := cfg.MTU
	if mtu == 0 {
		mtu = 1420
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{cfg.Address}, nil, mtu)
	if err != nil {
		return nil, fmt.Errorf("netstack: %w", err)
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), &device.Logger{
		Verbosef: func(f string, a ...any) { log.Debug(fmt.Sprintf(f, a...), "component", "wireguard") },
		Errorf:   func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...), "component", "wireguard") },
	})
	ipc := fmt.Sprintf("private_key=%s\nlisten_port=%d\n", cfg.PrivateKey.hex(), cfg.ListenPort)
	if err := dev.IpcSet(ipc); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configurando wireguard: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, err
	}
	return &Net{addr: cfg.Address, dev: dev, tnet: tnet, peers: map[Key]Peer{}}, nil
}

// SetPeers reemplaza el conjunto de peers aplicando solo las diferencias, así
// los peers que no cambian conservan su sesión.
func (n *Net) SetPeers(peers []Peer) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	want := make(map[Key]Peer, len(peers))
	for _, p := range peers {
		want[p.PublicKey] = p
	}
	var b strings.Builder
	for k := range n.peers {
		if _, ok := want[k]; !ok {
			fmt.Fprintf(&b, "public_key=%s\nremove=true\n", k.hex())
		}
	}
	for k, p := range want {
		if old, ok := n.peers[k]; ok && old == p {
			continue
		}
		fmt.Fprintf(&b, "public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s\npersistent_keepalive_interval=%d\n",
			k.hex(), p.AllowedIP, p.Keepalive)
		if p.Endpoint.IsValid() {
			fmt.Fprintf(&b, "endpoint=%s\n", p.Endpoint)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	if err := n.dev.IpcSet(b.String()); err != nil {
		return err
	}
	n.peers = want
	return nil
}

func (n *Net) Dial(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
	return n.tnet.DialContextTCPAddrPort(ctx, dst)
}

func (n *Net) Listen(port uint16) (net.Listener, error) {
	return n.tnet.ListenTCPAddrPort(netip.AddrPortFrom(n.addr, port))
}

func (n *Net) Close() { n.dev.Close() }

// LastHandshakes devuelve el último handshake exitoso de cada peer.
func (n *Net) LastHandshakes() map[Key]time.Time {
	out := map[Key]time.Time{}
	s, err := n.dev.IpcGet()
	if err != nil {
		return out
	}
	var cur Key
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "public_key":
			b, _ := hex.DecodeString(v)
			copy(cur[:], b)
		case "last_handshake_time_sec":
			if sec, _ := strconv.ParseInt(v, 10, 64); sec > 0 {
				out[cur] = time.Unix(sec, 0)
			}
		}
	}
	return out
}

// ResolveEndpoint resuelve "host:puerto" a una IP concreta (IPv4 primero):
// wireguard-go no acepta hostnames como endpoint.
func ResolveEndpoint(ctx context.Context, hostport string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return netip.AddrPort{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	best := ips[0]
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			best = ip
			break
		}
	}
	return netip.AddrPortFrom(best.Unmap(), uint16(p)), nil
}
