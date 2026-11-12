package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Hub sends Server-Sent Events to every open page.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newHub() *Hub { return &Hub{clients: map[chan []byte]struct{}{}} }

func sseFrame(event string, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte("{}")
	}
	return fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, b)
}

func (h *Hub) Publish(event string, v any) {
	frame := sseFrame(event, v)
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c <- frame:
		default:
			// A page this far behind is gone or asleep. It reconnects.
			delete(h.clients, c)
			close(c)
		}
	}
}

// Serve streams events. first is sent before anything else so a fresh page
// has the full state right away.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, first []byte) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx, NPM: do not buffer
	w.WriteHeader(http.StatusOK)

	c := make(chan []byte, 64)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if _, ok := h.clients[c]; ok {
			delete(h.clients, c)
			close(c)
		}
		h.mu.Unlock()
	}()

	if _, err := w.Write(append([]byte("retry: 3000\n\n"), first...)); err != nil {
		return
	}
	_ = rc.Flush()

	keep := time.NewTicker(20 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-c:
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
		case <-keep.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
