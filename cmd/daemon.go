package cmd

import (
	"log"

	"github.com/leoadberg/intermcp/daemon"
)

func Daemon() {
	d := daemon.New()
	log.Fatal(d.ListenAndServe())
}
