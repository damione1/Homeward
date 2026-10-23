package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/damione1/Homeward/internal/readonlyobservation"
)

func main() { os.Exit(run()) }
func run() int {
	flags := flag.NewFlagSet("homeward-observe", flag.ContinueOnError)
	configPath := flags.String("config", "", "protected observation JSON configuration")
	output := flags.String("out", "", "persistent evidence directory")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 || *configPath == "" || *output == "" {
		return 2
	}
	configuration, err := readonlyobservation.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "observation configuration rejected")
		return 1
	}
	source, err := readonlyobservation.NewNativeSource(configuration)
	if err != nil {
		fmt.Fprintln(os.Stderr, "observation transport unavailable")
		return 1
	}
	store, err := readonlyobservation.OpenStore(*output)
	if err != nil {
		fmt.Fprintln(os.Stderr, "observation evidence storage unavailable")
		return 1
	}
	defer store.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := readonlyobservation.Run(ctx, store, source); err != nil {
		fmt.Fprintln(os.Stderr, "observation incomplete; inspect persistent evidence")
		return 1
	}
	fmt.Fprintln(os.Stdout, "observation complete; inspect persistent evidence")
	return 0
}
