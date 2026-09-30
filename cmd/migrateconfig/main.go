package main

import (
	"flag"
	"log"

	"aisense/internal/config"
)

func main() {
	path := flag.String("config", "config.json", "path to the aisense config file")
	flag.Parse()
	if _, err := config.Load(*path); err != nil {
		log.Fatal(err)
	}
	log.Printf("settings and credentials are stored separately for %s", *path)
}
