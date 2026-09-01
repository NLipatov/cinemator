package main

import (
	"log"

	"cinemator/config"
	"cinemator/web"
)

// version is replaced with the release tag through -ldflags.
var version = "dev-build"

func main() {
	serverConfig := config.Load()
	server, newServerErr := web.NewServer(serverConfig, version)
	if newServerErr != nil {
		log.Fatalf("failed to init server: %v", newServerErr)
	}

	serveErr := server.Run()
	if serveErr != nil {
		log.Fatalf("server stopped: %v", serveErr)
	}
}
