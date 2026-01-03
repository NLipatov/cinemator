package main

import (
	"cinemator/presentation/settings"
	"cinemator/presentation/web/api"
	"log"
)

func main() {
	serverSettings := settings.NewSettings()
	server, newServerErr := api.NewHttpServer(serverSettings)
	if newServerErr != nil {
		log.Fatalf("failed to init server: %v", newServerErr)
	}

	serveErr := server.Run()
	if serveErr != nil {
		log.Fatalf("server stopped: %v", serveErr)
	}
}
