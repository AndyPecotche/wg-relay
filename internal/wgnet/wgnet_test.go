package wgnet

import (
	"context"
	"io"
	"net/netip"
	"testing"
	"time"
)

// Dos pilas en el mismo proceso, conectadas por UDP en localhost.
func TestTunnelEndToEnd(t *testing.T) {
	nodeKey, agentKey := GenerateKey(), GenerateKey()
	nodeIP, agentIP := netip.MustParseAddr("10.10.1.1"), netip.MustParseAddr("10.64.0.2")

	node, err := New(Config{PrivateKey: nodeKey, Address: nodeIP, ListenPort: 51899})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	agent, err := New(Config{PrivateKey: agentKey, Address: agentIP})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	must(t, node.SetPeers([]Peer{{PublicKey: agentKey.Public(), AllowedIP: netip.PrefixFrom(agentIP, 32)}}))
	must(t, agent.SetPeers([]Peer{{
		PublicKey: nodeKey.Public(), AllowedIP: netip.PrefixFrom(nodeIP, 32),
		Endpoint: netip.MustParseAddrPort("127.0.0.1:51899"), Keepalive: 1,
	}}))

	ln, err := agent.Listen(443)
	must(t, err)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("hola desde el agente"))
		c.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := node.Dial(ctx, netip.AddrPortFrom(agentIP, 443))
	must(t, err)
	got, err := io.ReadAll(c)
	must(t, err)
	if string(got) != "hola desde el agente" {
		t.Fatalf("recibido %q", got)
	}
	if _, ok := node.LastHandshakes()[agentKey.Public()]; !ok {
		t.Fatal("el nodo no registra handshake con el agente")
	}

	// Quitar el peer debe cortar la conectividad.
	must(t, node.SetPeers(nil))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := node.Dial(ctx2, netip.AddrPortFrom(agentIP, 443)); err == nil {
		t.Fatal("dial debería fallar sin peer")
	}
}

func TestKeyRoundTrip(t *testing.T) {
	k := GenerateKey()
	p, err := ParseKey(k.String())
	must(t, err)
	if p != k {
		t.Fatal("round trip")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
