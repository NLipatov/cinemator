package main

import (
	"cinemator/presentation/settings"
	"cinemator/presentation/web/api"
	"log"
)

// version is replaced with the release tag through -ldflags.
var version = "dev-build"

func main() {
	serverSettings := settings.NewSettings()
	server, newServerErr := api.NewHttpServer(serverSettings, version)
	if newServerErr != nil {
		log.Fatalf("failed to init server: %v", newServerErr)
	}

	serveErr := server.Run()
	if serveErr != nil {
		log.Fatalf("server stopped: %v", serveErr)
	}
}
