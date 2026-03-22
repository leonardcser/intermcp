package main

import (
	"fmt"
	"os"

	"github.com/leoadberg/intermcp/cmd"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: intermcp <daemon|serve>")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "daemon":
		cmd.Daemon()
	case "serve":
		cmd.Serve()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
