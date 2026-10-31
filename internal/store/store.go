// Package store keeps daemon state in SQLite: images, VMs, snapshots and
// network slot allocation. Files (disks, memory, vmstate) live on the storage
// path; this only records metadata and relationships.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/rrhubenov/dune/internal/model"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrHasChildren = errors.New("snapshot has children")
	ErrNoNetSlots  = errors.New("no free network slots")
)

// MaxNetSlot bounds slot numbers: slot N owns 10.200.0.0/16 offset N*4 (/30).
const MaxNetSlot = 16383

const (
	VMRunning = model.VMRunning
	VMPaused  = model.VMPaused
	VMStopped = model.VMStopped

	SnapFull = model.SnapFull
	SnapDiff = model.SnapDiff
)

type (
	Image    = model.Image
	VM       = model.VM
	Snapshot = model.Snapshot
)

type Store struct {
	db *sql.DB
	// MinSlot is the lowest network slot CreateVM hands out (default 1).
	// Tests raise it so their VMs never share a slot with a real daemon's.
	MinSlot int
}

const schema = `
CREATE TABLE images (
	name       TEXT PRIMARY KEY,
	kernel     TEXT NOT NULL,
	rootfs     TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE vms (
	id         TEXT PRIMARY KEY,
	image      TEXT NOT NULL REFERENCES images(name),
	state      TEXT NOT NULL,
	vcpus      INTEGER NOT NULL,
	mem_mib    INTEGER NOT NULL,
	net_slot   INTEGER NOT NULL UNIQUE,
	pid        INTEGER NOT NULL DEFAULT 0,
	origin     TEXT NOT NULL DEFAULT '',
	head       TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE snapshots (
	id         TEXT PRIMARY KEY,
	vm_id      TEXT NOT NULL,
	parent     TEXT NOT NULL DEFAULT '',
	kind       TEXT NOT NULL,
	mem_mib    INTEGER NOT NULL,
	vcpus      INTEGER NOT NULL,
	image      TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX snapshots_parent ON snapshots(parent);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One writer; avoids SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version == 0 {
		if _, err := db.Exec(schema + "PRAGMA user_version = 1;"); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db, MinSlot: 1}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func NewID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Images

func (s *Store) PutImage(im Image) error {
	_, err := s.db.Exec(`INSERT INTO images (name, kernel, rootfs, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET kernel = excluded.kernel, rootfs = excluded.rootfs`,
		im.Name, im.Kernel, im.Rootfs, im.CreatedAt.Unix())
	return err
}

func (s *Store) GetImage(name string) (Image, error) {
	var im Image
	var ts int64
	err := s.db.QueryRow(`SELECT name, kernel, rootfs, created_at FROM images WHERE name = ?`, name).
		Scan(&im.Name, &im.Kernel, &im.Rootfs, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return im, fmt.Errorf("image %s: %w", name, ErrNotFound)
	}
	im.CreatedAt = time.Unix(ts, 0)
	return im, err
}

func (s *Store) ListImages() ([]Image, error) {
	rows, err := s.db.Query(`SELECT name, kernel, rootfs, created_at FROM images ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var im Image
		var ts int64
		if err := rows.Scan(&im.Name, &im.Kernel, &im.Rootfs, &ts); err != nil {
			return nil, err
		}
		im.CreatedAt = time.Unix(ts, 0)
		out = append(out, im)
	}
	return out, rows.Err()
}

// VMs

// CreateVM inserts vm with a fresh ID and the lowest free network slot.
func (s *Store) CreateVM(vm VM) (VM, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return vm, err
	}
	defer tx.Rollback()
	// Lowest slot >= MinSlot not in use.
	var slot int
	err = tx.QueryRow(`SELECT MIN(n) FROM (
		SELECT ?1 AS n WHERE NOT EXISTS (SELECT 1 FROM vms WHERE net_slot = ?1)
		UNION ALL
		SELECT net_slot + 1 FROM vms WHERE net_slot >= ?1 AND net_slot + 1 NOT IN (SELECT net_slot FROM vms)
	)`, s.MinSlot).Scan(&slot)
	if err != nil {
		return vm, err
	}
	if slot > MaxNetSlot {
		return vm, ErrNoNetSlots
	}
	vm.ID = NewID()
	vm.NetSlot = slot
	vm.CreatedAt = time.Now()
	_, err = tx.Exec(`INSERT INTO vms (id, image, state, vcpus, mem_mib, net_slot, pid, origin, head, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		vm.ID, vm.Image, vm.State, vm.VCPUs, vm.MemMiB, vm.NetSlot, vm.PID, vm.Origin, vm.Head, vm.CreatedAt.Unix())
	if err != nil {
		return vm, err
	}
	return vm, tx.Commit()
}

const vmCols = `id, image, state, vcpus, mem_mib, net_slot, pid, origin, head, created_at`

func scanVM(sc interface{ Scan(...any) error }) (VM, error) {
	var vm VM
	var ts int64
	err := sc.Scan(&vm.ID, &vm.Image, &vm.State, &vm.VCPUs, &vm.MemMiB, &vm.NetSlot, &vm.PID, &vm.Origin, &vm.Head, &ts)
	vm.CreatedAt = time.Unix(ts, 0)
	return vm, err
}

func (s *Store) GetVM(id string) (VM, error) {
	vm, err := scanVM(s.db.QueryRow(`SELECT `+vmCols+` FROM vms WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return vm, fmt.Errorf("vm %s: %w", id, ErrNotFound)
	}
	return vm, err
}

func (s *Store) ListVMs() ([]VM, error) {
	rows, err := s.db.Query(`SELECT ` + vmCols + ` FROM vms ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VM
	for rows.Next() {
		vm, err := scanVM(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, vm)
	}
	return out, rows.Err()
}

func (s *Store) SetVMState(id, state string, pid int) error {
	return s.exec1(`UPDATE vms SET state = ?, pid = ? WHERE id = ?`, state, pid, id)
}

func (s *Store) SetVMHead(id, head string) error {
	return s.exec1(`UPDATE vms SET head = ? WHERE id = ?`, head, id)
}

func (s *Store) DeleteVM(id string) error {
	return s.exec1(`DELETE FROM vms WHERE id = ?`, id)
}

// Snapshots

func (s *Store) CreateSnapshot(sn Snapshot) (Snapshot, error) {
	sn.ID = NewID()
	sn.CreatedAt = time.Now()
	_, err := s.db.Exec(`INSERT INTO snapshots (id, vm_id, parent, kind, mem_mib, vcpus, image, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sn.ID, sn.VMID, sn.Parent, sn.Kind, sn.MemMiB, sn.VCPUs, sn.Image, sn.CreatedAt.Unix())
	return sn, err
}

const snapCols = `id, vm_id, parent, kind, mem_mib, vcpus, image, created_at`

func scanSnap(sc interface{ Scan(...any) error }) (Snapshot, error) {
	var sn Snapshot
	var ts int64
	err := sc.Scan(&sn.ID, &sn.VMID, &sn.Parent, &sn.Kind, &sn.MemMiB, &sn.VCPUs, &sn.Image, &ts)
	sn.CreatedAt = time.Unix(ts, 0)
	return sn, err
}

func (s *Store) GetSnapshot(id string) (Snapshot, error) {
	sn, err := scanSnap(s.db.QueryRow(`SELECT `+snapCols+` FROM snapshots WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return sn, fmt.Errorf("snapshot %s: %w", id, ErrNotFound)
	}
	return sn, err
}

func (s *Store) ListSnapshots() ([]Snapshot, error) {
	rows, err := s.db.Query(`SELECT ` + snapCols + ` FROM snapshots ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		sn, err := scanSnap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// Chain returns the delta chain ending at id, base (full) snapshot first.
func (s *Store) Chain(id string) ([]Snapshot, error) {
	var chain []Snapshot
	for id != "" {
		sn, err := s.GetSnapshot(id)
		if err != nil {
			return nil, err
		}
		chain = append([]Snapshot{sn}, chain...)
		id = sn.Parent
	}
	if len(chain) == 0 || chain[0].Kind != SnapFull {
		return nil, fmt.Errorf("snapshot chain does not start with a full snapshot")
	}
	return chain, nil
}

// DeleteSnapshot refuses while other snapshots build on it or VMs track it as
// head (their next diff would need it).
func (s *Store) DeleteSnapshot(id string) error {
	var n int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM snapshots WHERE parent = ?) +
		(SELECT COUNT(*) FROM vms WHERE head = ?)`, id, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("snapshot %s: %w", id, ErrHasChildren)
	}
	return s.exec1(`DELETE FROM snapshots WHERE id = ?`, id)
}

func (s *Store) exec1(q string, args ...any) error {
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
