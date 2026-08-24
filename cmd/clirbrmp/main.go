// Command clirbrmp is the admin CLI for a running rbrmp-server: a thin client
// for its RCON console (plain TCP, line-based, password-protected).
//
//	clirbrmp players                        one shot: run a command, exit
//	clirbrmp kick 3 flooding the chat
//	clirbrmp -addr host:40101 status
//	clirbrmp                                interactive: a rbrmp> prompt
//
// The password comes from -password or, failing that, the RBRMP_RCON_PASSWORD
// environment variable - the same one the server reads.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const helpText = `commands:
  players              list connected players
  kick <id|name> [reason...]
  ban <id|name|ip> [reason...]
  unban <ip>
  bans                 list ban entries
  say <text...>        broadcast a chat line as SERVER
  status               server address, uptime, tick rate, traffic
  help                 this text (local, not sent to the server)
  exit | quit          leave`

func main() {
	addr := flag.String("addr", "127.0.0.1:40101", "RCON address of the server")
	password := flag.String("password", "",
		"RCON password (env RBRMP_RCON_PASSWORD is the fallback)")
	flag.Parse()

	pw := *password
	if pw == "" {
		pw = os.Getenv("RBRMP_RCON_PASSWORD")
	}
	if pw == "" {
		fmt.Fprintln(os.Stderr, "no password: use -password or set RBRMP_RCON_PASSWORD")
		os.Exit(1)
	}

	conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 64*1024)

	if _, err := fmt.Fprintf(conn, "AUTH %s\n", pw); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := response(sc, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if args := flag.Args(); len(args) > 0 {
		// One-shot: run the command, print its payload, exit by verdict.
		if err := run(conn, sc, strings.Join(args, " "), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	// Interactive.
	fmt.Printf("connected to %s - 'help' lists commands, 'exit' leaves\n", *addr)
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("rbrmp> ")
		if !in.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(in.Text())
		switch {
		case line == "":
			continue
		case line == "help":
			fmt.Println(helpText)
			continue
		case line == "exit" || line == "quit":
			run(conn, sc, "quit", os.Stdout)
			return
		}
		if err := run(conn, sc, line, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			if _, netErr := err.(net.Error); netErr || err == errClosed {
				return // the server hung up; nothing more to type at
			}
		}
	}
}

var errClosed = fmt.Errorf("connection closed by server")

// run sends one command and prints the payload lines, "| " stripped. The
// terminator decides the error: OK is nil, ERR is its message.
func run(conn net.Conn, sc *bufio.Scanner, line string, out *os.File) error {
	if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
		return err
	}
	_, err := response(sc, out)
	return err
}

// response reads payload lines up to the OK/ERR terminator, writing them to
// out (nil discards them, for AUTH).
func response(sc *bufio.Scanner, out *os.File) (int, error) {
	n := 0
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "OK":
			return n, nil
		case strings.HasPrefix(line, "ERR "):
			return n, fmt.Errorf("%s", strings.TrimPrefix(line, "ERR "))
		default:
			if out != nil {
				fmt.Fprintln(out, strings.TrimPrefix(line, "| "))
			}
			n++
		}
	}
	if err := sc.Err(); err != nil {
		return n, err
	}
	return n, errClosed
}
