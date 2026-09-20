// wgrelay-node es un nodo relay: la cara pública del servicio.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/AndyPecotche/wg-relay/internal/node"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	local, err := node.ParseLocalRoutes(os.Getenv("WGRELAY_LOCAL_ROUTES"))
	if err != nil {
		fatal(err)
	}
	port, err := strconv.Atoi(env("WGRELAY_WG_PORT", "51820"))
	if err != nil {
		fatal(fmt.Errorf("WGRELAY_WG_PORT: %w", err))
	}
	cfg := node.Config{
		APIURL:      os.Getenv("WGRELAY_API_URL"),
		Token:       os.Getenv("WGRELAY_NODE_TOKEN"),
		WGPort:      port,
		HTTPSListen: env("WGRELAY_HTTPS_LISTEN", ":443"),
		HTTPListen:  env("WGRELAY_HTTP_LISTEN", ":80"),
		DNSListen:   env("WGRELAY_DNS_LISTEN", ""),
		LocalRoutes: local,
		Log:         log,
	}
	if cfg.APIURL == "" || cfg.Token == "" {
		fatal(fmt.Errorf("WGRELAY_API_URL y WGRELAY_NODE_TOKEN son obligatorias"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := node.Run(ctx, cfg); err != nil {
		fatal(err)
	}
}

func logLevel() slog.Level {
	if os.Getenv("WGRELAY_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
