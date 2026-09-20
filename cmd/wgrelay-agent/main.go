// wgrelay-agent expone servicios locales a través de wg-relay.
//
// Solo necesita dos cosas: la variable WGRELAY_TOKEN y un wgrelay.yml (que se
// puede versionar con el proyecto, porque no contiene secretos).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/AndyPecotche/wg-relay/internal/agent"
)

func main() {
	path := flag.String("config", env("WGRELAY_CONFIG", "wgrelay.yml"), "ruta a wgrelay.yml")
	flag.Parse()

	level := slog.LevelInfo
	if os.Getenv("WGRELAY_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	file, err := agent.LoadFile(*path)
	if err != nil {
		fatal(err)
	}
	if v := os.Getenv("WGRELAY_RELAY"); v != "" {
		file.Relay = v
	}
	if file.Relay == "" {
		fatal(fmt.Errorf("falta la URL del relay: definí relay: en %s o WGRELAY_RELAY", *path))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.Run(ctx, agent.Config{Token: os.Getenv("WGRELAY_TOKEN"), File: file, Log: log}); err != nil {
		fatal(err)
	}
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
