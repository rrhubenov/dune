package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/rrhubenov/dune/internal/model"
)

// events streams change notifications ({"topic":"vms"} etc.) to the UI.
// websocket.Accept rejects cross-origin upgrades by default.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if s.Events == nil {
		writeJSON(w, http.StatusNotImplemented, Error{Error: "no event hub"})
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context())
	sub, unsubscribe := s.Events.Subscribe()
	defer unsubscribe()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-sub:
			b, _ := json.Marshal(ev)
			if err := c.Write(ctx, websocket.MessageText, b); err != nil {
				return
			}
		case <-ping.C:
			if err := c.Ping(ctx); err != nil {
				return
			}
		}
	}
}

type TerminalControl = model.TerminalControl

// terminal bridges a WebSocket to a login shell in the VM: binary messages
// are terminal bytes in both directions, text messages are TerminalControl.
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cols, rows := dim(q.Get("cols"), 80), dim(q.Get("rows"), 24)
	pty, err := s.VMs.OpenPTY(r.Context(), r.PathValue("id"), cols, rows)
	if err != nil {
		fail(w, err)
		return
	}
	defer pty.Close()
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() { // client -> shell
		defer cancel()
		for {
			typ, msg, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := pty.Write(msg); err != nil {
					return
				}
				continue
			}
			var ctl TerminalControl
			if json.Unmarshal(msg, &ctl) == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
				pty.Resize(ctl.Cols, ctl.Rows)
			}
		}
	}()
	go func() { // unblock pty.Next when the client leaves
		<-ctx.Done()
		pty.Close()
	}()
	for { // shell -> client
		data, exited, code, err := pty.Next()
		if err != nil {
			c.Close(websocket.StatusGoingAway, "terminal closed")
			return
		}
		if exited {
			b, _ := json.Marshal(TerminalControl{Type: "exit", Code: code})
			c.Write(ctx, websocket.MessageText, b)
			c.Close(websocket.StatusNormalClosure, "")
			return
		}
		if err := c.Write(ctx, websocket.MessageBinary, data); err != nil {
			return
		}
	}
}

func dim(s string, def uint16) uint16 {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return def
	}
	return uint16(n)
}
