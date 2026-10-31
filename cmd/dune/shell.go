package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/coder/websocket"
	"golang.org/x/term"

	"github.com/rrhubenov/dune/internal/client"
	"github.com/rrhubenov/dune/internal/model"
)

// cmdShell opens an interactive login shell in a VM, like ssh: the local
// terminal goes raw and follows window resizes. Exits with the shell's code.
func cmdShell(c *client.Client, args []string) error {
	id, err := one(args, "shell ID")
	if err != nil {
		return err
	}
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	cols, rows := 80, 24
	if w, h, err := term.GetSize(out); err == nil {
		cols, rows = w, h
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := c.Terminal(ctx, id, cols, rows)
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	if term.IsTerminal(in) {
		old, err := term.MakeRaw(in)
		if err != nil {
			return err
		}
		defer term.Restore(in, old)
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if w, h, err := term.GetSize(out); err == nil {
				b, _ := json.Marshal(model.TerminalControl{Type: "resize", Cols: uint16(w), Rows: uint16(h)})
				conn.Write(ctx, websocket.MessageText, b)
			}
		}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && conn.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		typ, msg, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return nil
			}
			return fmt.Errorf("terminal: %w", err)
		}
		if typ == websocket.MessageBinary {
			os.Stdout.Write(msg)
			continue
		}
		var ctl model.TerminalControl
		if json.Unmarshal(msg, &ctl) == nil && ctl.Type == "exit" {
			if ctl.Code != 0 {
				conn.Close(websocket.StatusNormalClosure, "")
				cancel()
				return exitCode(ctl.Code)
			}
			return nil
		}
	}
}

// exitCode makes main exit with a given status (after deferred cleanup,
// e.g. restoring the terminal, has run).
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func asExitCode(err error) (int, bool) {
	var e exitCode
	if errors.As(err, &e) {
		return int(e), true
	}
	return 0, false
}
