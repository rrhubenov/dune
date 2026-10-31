package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const defaultConfigPath = "/etc/dune/duned.json"

// Config is read from a JSON file; every field has a default, so the file
// can be absent or partial.
type Config struct {
	DataDir    string `json:"data_dir"`    // default /srv/dune
	StoreImage string `json:"store_image"` // default <data_dir>/store.img
	StoreMount string `json:"store_mount"` // default <data_dir>/store
	StoreSize  string `json:"store_size"`  // default 500G (sparse)
	DB         string `json:"db"`          // default <data_dir>/dune.db

	// Firecracker release binaries. Default <data_dir>/bin/{firecracker,jailer,snapshot-editor}.
	Firecracker    string `json:"firecracker"`
	Jailer         string `json:"jailer"`
	SnapshotEditor string `json:"snapshot_editor"`

	Socket string `json:"socket"` // default /run/dune/duned.sock (root only)
	// Listen is the tailnet address: "tailscale" (default) binds this
	// host's Tailscale IPv4 on port 7070, "" disables, or an explicit host:port.
	Listen string `json:"listen"`
	// AllowUsers are the Tailscale logins allowed on the tailnet listener.
	AllowUsers []string `json:"allow_users"`

	UIDBase int `json:"uid_base"` // default 100000; VMs run as uid_base+slot
}

func loadConfig(path string) (Config, error) {
	c := Config{Listen: "tailscale"}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && path == defaultConfigPath:
	case err != nil:
		return c, err
	default:
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&c.DataDir, "/srv/dune")
	def(&c.StoreImage, filepath.Join(c.DataDir, "store.img"))
	def(&c.StoreMount, filepath.Join(c.DataDir, "store"))
	def(&c.StoreSize, "500G")
	def(&c.DB, filepath.Join(c.DataDir, "dune.db"))
	def(&c.Firecracker, filepath.Join(c.DataDir, "bin", "firecracker"))
	def(&c.Jailer, filepath.Join(c.DataDir, "bin", "jailer"))
	def(&c.SnapshotEditor, filepath.Join(c.DataDir, "bin", "snapshot-editor"))
	def(&c.Socket, "/run/dune/duned.sock")
	if c.UIDBase == 0 {
		c.UIDBase = 100000
	}
	return c, nil
}
