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
		log.Println(newServerErr)
	}

	serveErr := server.Run()
	if serveErr != nil {
		log.Println(serveErr)
	}
}
