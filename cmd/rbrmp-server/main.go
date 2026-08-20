// Command rbrmp-server is the RBR-MP multiplayer server.
//
// It relays car poses between clients, and - the part that makes it useful
// before there is a second player to test with - it can replay each client
// back to itself on a delay, so one person driving alone sees a second car
// doing exactly what they did a second ago.
//
//	rbrmp-server                     listen on :40100
//	rbrmp-server -addr :7777 -v      another port, log every oddity
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lukaszryczko/rbr-mp-server/internal/server"
)

func main() {
	cfg := server.DefaultConfig()

	flag.StringVar(&cfg.Addr, "addr", cfg.Addr, "UDP address to listen on")
	tickHz := flag.Int("tick", 30, "snapshots per second sent to each client")
	flag.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "drop a client silent for this long")
	flag.DurationVar(&cfg.Stale, "stale", cfg.Stale,
		"stop relaying a player whose newest state is older than this")
	flag.DurationVar(&cfg.Stats, "stats", cfg.Stats, "how often to print a traffic line (0 = never)")
	flag.BoolVar(&cfg.Verbose, "v", false, "log malformed datagrams and send errors")
	flag.Parse()

	if *tickHz <= 0 || *tickHz > 240 {
		fmt.Fprintln(os.Stderr, "-tick must be between 1 and 240")
		os.Exit(2)
	}
	cfg.TickAt = time.Second / time.Duration(*tickHz)

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	srv, err := server.New(cfg, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Ctrl-C closes the socket, which makes Run return.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		logger.Println("shutting down")
		srv.Close()
	}()

	// A closed socket is how a Ctrl-C shutdown gets out of Run; anything else
	// is a real failure.
	if err := srv.Run(); err != nil && !errors.Is(err, net.ErrClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
