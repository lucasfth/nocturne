// Command nocturne is the Go port of the nocturne tooling.
package main

import (
	"context"
	"fmt"
	gateway "github.com/lucasfth/nocturne/go/internal/runtime"
	"os"
	"os/signal"
	"syscall"

	"github.com/lucasfth/nocturne/go/internal/tunnelkeys"
)

const usage = "usage: nocturne {serve,sync-tunnel-keys} [args...]\n"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		if len(args) != 1 {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		r, err := gateway.New(os.Getenv("SERVICE_ROOT"), gateway.Options{})
		if err == nil {
			err = r.Serve(ctx)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			return 1
		}
		return 0
	case "sync-tunnel-keys":
		return tunnelkeys.Main(args[1:], os.Stdout, os.Stderr)
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "nocturne: unknown command %q\n%s", args[0], usage)
		return 2
	}
}
