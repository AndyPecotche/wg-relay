package sni

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

func TestPeekReplays(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		c := tls.Client(client, &tls.Config{ServerName: "Mqtt.Example.COM", InsecureSkipVerify: true})
		_ = c.Handshake() // falla: nadie responde; solo queremos el hello
	}()

	name, conn, err := Peek(server, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if name != "mqtt.example.com" {
		t.Fatalf("SNI = %q", name)
	}
	// Los primeros bytes reproducidos deben ser un registro TLS handshake.
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatal(err)
	}
	if hdr[0] != 0x16 {
		t.Fatalf("primer byte = %#x, se esperaba 0x16", hdr[0])
	}
	client.Close()
}

func TestPeekNotTLS(t *testing.T) {
	client, server := net.Pipe()
	go func() { client.Write([]byte("GET / HTTP/1.1\r\n\r\n")); client.Close() }()
	if _, _, err := Peek(server, time.Second); err == nil {
		t.Fatal("debería fallar con tráfico no TLS")
	}
}

func TestMatch(t *testing.T) {
	table := map[string]int{"abc.clients.x": 1, "*.abc.clients.x": 2, "exacto.com": 3}
	cases := map[string]int{
		"abc.clients.x":     1,
		"app.abc.clients.x": 2,
		"a.b.abc.clients.x": 2,
		"exacto.com":        3,
		"otro.exacto.com":   0,
		"zzz.clients.x":     0,
	}
	for host, want := range cases {
		got, ok := Match(table, host)
		if (want == 0) == ok || got != want {
			t.Errorf("Match(%q) = %d,%v; want %d", host, got, ok, want)
		}
	}
}
