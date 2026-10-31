// Package api is duned's HTTP+JSON API (v1). The same handler serves the
// local unix socket and the tailnet listener; the latter is wrapped in a
// Tailscale identity check (see WhoisAuth).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/rrhubenov/dune/internal/backend"
	"github.com/rrhubenov/dune/internal/model"
	"github.com/rrhubenov/dune/internal/storage"
	"github.com/rrhubenov/dune/internal/store"
	"github.com/rrhubenov/dune/internal/vm"
)

type (
	ImportImageRequest = model.ImportImageRequest
	CreateVMRequest    = model.CreateVMRequest
	PinRequest         = model.PinRequest
	ForkRequest        = model.ForkRequest
	ForkResponse       = model.ForkResponse
	ExecRequest        = model.ExecRequest
	Error              = model.Error
)

// Console is implemented by backends with a serial console.
// BETA: replaced by a vsock guest agent later.
type Console interface {
	ConsolePath(id string) string
	ConsoleInput(id string) io.Writer
}

type Server struct {
	VMs     *vm.Manager
	Storage *storage.Store
	Console Console // optional
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/storage", s.getStorage)
	mux.HandleFunc("POST /v1/storage/trim", s.trimStorage)
	mux.HandleFunc("GET /v1/images", s.listImages)
	mux.HandleFunc("POST /v1/images", s.importImage)
	mux.HandleFunc("GET /v1/vms", s.listVMs)
	mux.HandleFunc("POST /v1/vms", s.createVM)
	mux.HandleFunc("GET /v1/vms/{id}", s.getVM)
	mux.HandleFunc("DELETE /v1/vms/{id}", s.deleteVM)
	mux.HandleFunc("POST /v1/vms/{id}/pause", s.pauseVM)
	mux.HandleFunc("POST /v1/vms/{id}/resume", s.resumeVM)
	mux.HandleFunc("POST /v1/vms/{id}/snapshots", s.pinVM)
	mux.HandleFunc("POST /v1/vms/{id}/exec", s.execVM)
	mux.HandleFunc("GET /v1/vms/{id}/console", s.readConsole)
	mux.HandleFunc("POST /v1/vms/{id}/console", s.writeConsole)
	mux.HandleFunc("GET /v1/snapshots", s.listSnapshots)
	mux.HandleFunc("DELETE /v1/snapshots/{id}", s.deleteSnapshot)
	mux.HandleFunc("POST /v1/snapshots/{id}/fork", s.forkSnapshot)
	return logRequests(mux)
}

// ctx detaches long operations from the client connection: a fork or
// snapshot that the client stops waiting for still completes cleanly.
func ctx(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

func (s *Server) getStorage(w http.ResponseWriter, r *http.Request) {
	u, err := s.Storage.Usage()
	reply(w, u, err)
}

func (s *Server) trimStorage(w http.ResponseWriter, r *http.Request) {
	if err := s.Storage.Trim(); err != nil {
		fail(w, err)
		return
	}
	u, err := s.Storage.Usage()
	reply(w, u, err)
}

func (s *Server) listImages(w http.ResponseWriter, r *http.Request) {
	ims, err := s.VMs.Images()
	reply(w, nonNil(ims), err)
}

func (s *Server) importImage(w http.ResponseWriter, r *http.Request) {
	var req ImportImageRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Name == "" || req.Kernel == "" || req.Rootfs == "" {
		writeJSON(w, http.StatusBadRequest, Error{Error: "name, kernel and rootfs are required"})
		return
	}
	im, err := s.VMs.ImportImage(req.Name, req.Kernel, req.Rootfs)
	reply(w, im, err)
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.VMs.List()
	reply(w, nonNil(vms), err)
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	var req CreateVMRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := s.VMs.Create(ctx(r), req)
	reply(w, v, err)
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request) {
	v, err := s.VMs.Get(r.PathValue("id"))
	reply(w, v, err)
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	if err := s.VMs.Delete(ctx(r), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pauseVM(w http.ResponseWriter, r *http.Request) {
	v, err := s.VMs.Pause(ctx(r), r.PathValue("id"))
	reply(w, v, err)
}

func (s *Server) resumeVM(w http.ResponseWriter, r *http.Request) {
	v, err := s.VMs.Resume(ctx(r), r.PathValue("id"))
	reply(w, v, err)
}

func (s *Server) pinVM(w http.ResponseWriter, r *http.Request) {
	var req PinRequest
	if !decode(w, r, &req) {
		return
	}
	kind := backend.SnapshotKind(req.Kind)
	if kind != "" && kind != backend.Full && kind != backend.Diff {
		writeJSON(w, http.StatusBadRequest, Error{Error: `kind must be "full" or "diff"`})
		return
	}
	sn, err := s.VMs.Pin(ctx(r), r.PathValue("id"), kind)
	reply(w, sn, err)
}

// execVM runs a command via the guest agent. Unlike other operations it is
// tied to the client connection: if the client goes away, the command is
// killed.
func (s *Server) execVM(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.VMs.Exec(r.Context(), r.PathValue("id"), req)
	reply(w, res, err)
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	sns, err := s.VMs.Snapshots()
	reply(w, nonNil(sns), err)
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.VMs.DeleteSnapshot(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) forkSnapshot(w http.ResponseWriter, r *http.Request) {
	var req ForkRequest
	if !decode(w, r, &req) {
		return
	}
	vms, err := s.VMs.Fork(ctx(r), r.PathValue("id"), req.Count)
	if err != nil && len(vms) > 0 {
		writeJSON(w, http.StatusInternalServerError, ForkResponse{VMs: vms, Error: err.Error()})
		return
	}
	reply(w, ForkResponse{VMs: nonNil(vms)}, err)
}

// readConsole returns the last ?tail= bytes (default 16 KiB) of the serial log.
func (s *Server) readConsole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.VMs.Get(id); err != nil {
		fail(w, err)
		return
	}
	if s.Console == nil {
		writeJSON(w, http.StatusNotImplemented, Error{Error: "backend has no console"})
		return
	}
	n := int64(16 << 10)
	if t := r.URL.Query().Get("tail"); t != "" {
		v, err := strconv.ParseInt(t, 10, 64)
		if err != nil || v <= 0 {
			writeJSON(w, http.StatusBadRequest, Error{Error: "tail must be a positive integer"})
			return
		}
		n = v
	}
	f, err := os.Open(s.Console.ConsolePath(id))
	if err != nil {
		fail(w, err)
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > n {
		f.Seek(fi.Size()-n, io.SeekStart)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.Copy(w, f)
}

// writeConsole types the request body on the guest's serial console.
func (s *Server) writeConsole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.VMs.Get(id); err != nil {
		fail(w, err)
		return
	}
	var in io.Writer
	if s.Console != nil {
		in = s.Console.ConsoleInput(id)
	}
	if in == nil {
		writeJSON(w, http.StatusConflict, Error{Error: "console input unavailable (VM not started by this daemon instance)"})
		return
	}
	if _, err := io.Copy(in, io.LimitReader(r.Body, 64<<10)); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) { // empty body = all defaults
		writeJSON(w, http.StatusBadRequest, Error{Error: "bad JSON: " + err.Error()})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, vm.ErrState), errors.Is(err, store.ErrHasChildren):
		status = http.StatusConflict
	case errors.Is(err, store.ErrNoNetSlots):
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, Error{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// nonNil makes empty lists encode as [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// Successful reads are polled (e.g. console by dune exec); skip them.
		if r.Method == http.MethodGet && sw.status < 400 {
			return
		}
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "user", UserFrom(r.Context()))
	})
}
