package main

import (
	"context"
	"flag"
	"github.com/damione1/new-home-assistant/internal/config"
	"github.com/damione1/new-home-assistant/internal/server"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	path := flag.String("config", "config/example.yaml", "initial YAML configuration")
	flag.Parse()
	data, err := os.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	c, err := config.Parse(data)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var lc net.ListenConfig
	if err := server.Run(ctx, c, lc.Listen, func(a net.Addr) { log.Printf("bootstrap ready %s", a) }); err != nil {
		log.Fatal(err)
	}
	log.Print("shutdown complete")
}
