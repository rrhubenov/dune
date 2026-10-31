package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
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
