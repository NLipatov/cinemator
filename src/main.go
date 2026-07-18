package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"cinemator/presentation/settings"
	"cinemator/presentation/web/api"
)

func main() {
	serverSettings := settings.NewSettings()
	server, newServerErr := api.NewHttpServer(serverSettings)
	if newServerErr != nil {
		log.Fatalf("failed to init server: %v", newServerErr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serveErr := server.Run(ctx)
	if serveErr != nil {
		log.Fatalf("server stopped: %v", serveErr)
	}
}
