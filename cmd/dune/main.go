// dune is the command-line client for duned.
//
// It talks to the daemon at $DUNE_HOST: "unix:///run/dune/duned.sock" (the
// default when that socket exists; needs root) or "http://<host>:7070" over
// the tailnet.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rrhubenov/dune/internal/client"
	"github.com/rrhubenov/dune/internal/model"
)

const defaultSocket = "/run/dune/duned.sock"

const usage = `usage: dune <command> [args]

VMs:
  run [--image NAME] [--cpus N] [--mem MiB]   boot a new VM
  ls                                          list VMs
  pause ID... | resume ID...
  pin ID [--full|--diff]                      snapshot a VM (default: diff if it has one, else full)
  fork SNAP [-n N]                            start N VMs from a snapshot
  rm ID...                                    stop and delete VMs
  console ID [--tail BYTES]                   print serial console output
  exec ID CMD...                              run CMD on the serial console (BETA)

Snapshots:
  snap ls                                     snapshot tree, with VMs forked from each
  snap rm ID...

Images and storage:
  image ls
  image import NAME --kernel PATH --rootfs PATH   (paths on the daemon host)
  storage [trim]

Environment:
  DUNE_HOST   unix:///run/dune/duned.sock (default if present) or http://midas:7070
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	c, err := connect()
	if err == nil {
		err = dispatch(c, os.Args[1], os.Args[2:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dune:", err)
		os.Exit(1)
	}
}

func connect() (*client.Client, error) {
	host := os.Getenv("DUNE_HOST")
	if host == "" {
		if _, err := os.Stat(defaultSocket); err != nil {
			return nil, errors.New("no local daemon socket; set DUNE_HOST=http://<host>:7070")
		}
		host = "unix://" + defaultSocket
	}
	return client.New(host)
}

func dispatch(c *client.Client, cmd string, args []string) error {
	switch cmd {
	case "run":
		return cmdRun(c, args)
	case "ls", "ps":
		return cmdLs(c)
	case "pause", "resume":
		return cmdPauseResume(c, cmd, args)
	case "pin":
		return cmdPin(c, args)
	case "fork":
		return cmdFork(c, args)
	case "rm":
		return eachID(args, "rm ID...", c.DeleteVM)
	case "console":
		return cmdConsole(c, args)
	case "exec":
		return cmdExec(c, args)
	case "snap", "snaps":
		sub := "ls"
		if len(args) > 0 {
			sub, args = args[0], args[1:]
		}
		switch sub {
		case "ls":
			return cmdSnapLs(c)
		case "rm":
			return eachID(args, "snap rm ID...", c.DeleteSnapshot)
		}
		return fmt.Errorf("unknown: snap %s", sub)
	case "image", "images":
		sub := "ls"
		if len(args) > 0 {
			sub, args = args[0], args[1:]
		}
		switch sub {
		case "ls":
			return cmdImageLs(c)
		case "import":
			return cmdImageImport(c, args)
		}
		return fmt.Errorf("unknown: image %s", sub)
	case "storage":
		return cmdStorage(c, args)
	}
	return fmt.Errorf("unknown command %q (dune help)", cmd)
}

// parse parses flags that may appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func eachID(ids []string, use string, f func(string) error) error {
	if len(ids) == 0 {
		return errors.New("usage: dune " + use)
	}
	var errs []error
	for _, id := range ids {
		if err := f(id); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		fmt.Println(id)
	}
	return errors.Join(errs...)
}

func one(args []string, use string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("usage: dune " + use)
	}
	return args[0], nil
}

func cmdRun(c *client.Client, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	image := fs.String("image", "ubuntu", "image name")
	cpus := fs.Int("cpus", 1, "vCPUs")
	mem := fs.Int("mem", 1024, "memory in MiB")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	vm, err := c.CreateVM(model.CreateVMRequest{Image: *image, VCPUs: *cpus, MemMiB: *mem})
	if err != nil {
		return err
	}
	fmt.Println(vm.ID)
	return nil
}

func cmdLs(c *client.Client) error {
	vms, err := c.VMs()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tIMAGE\tCPUS\tMEM\tORIGIN\tHEAD\tAGE")
	for _, vm := range vms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%dM\t%s\t%s\t%s\n",
			vm.ID, vm.State, vm.Image, vm.VCPUs, vm.MemMiB, dash(vm.Origin), dash(vm.Head), age(vm.CreatedAt))
	}
	return w.Flush()
}

func cmdPauseResume(c *client.Client, cmd string, args []string) error {
	f := c.Pause
	if cmd == "resume" {
		f = c.Resume
	}
	return eachID(args, cmd+" ID...", func(id string) error {
		_, err := f(id)
		return err
	})
}

func cmdPin(c *client.Client, args []string) error {
	fs := flag.NewFlagSet("pin", flag.ContinueOnError)
	full := fs.Bool("full", false, "full snapshot")
	diff := fs.Bool("diff", false, "diff snapshot (falls back to full if the VM has no previous snapshot)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one(pos, "pin ID [--full|--diff]")
	if err != nil {
		return err
	}
	var kind string
	switch {
	case *full && *diff:
		return errors.New("--full and --diff are exclusive")
	case *full:
		kind = model.SnapFull
	case *diff:
		kind = model.SnapDiff
	}
	start := time.Now()
	sn, err := c.Pin(id, kind)
	if err != nil {
		return err
	}
	fmt.Printf("%s\t%s\t%s\n", sn.ID, sn.Kind, time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdFork(c *client.Client, args []string) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	n := fs.Int("n", 1, "number of VMs")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one(pos, "fork SNAP [-n N]")
	if err != nil {
		return err
	}
	vms, err := c.Fork(id, *n)
	for _, vm := range vms {
		fmt.Println(vm.ID)
	}
	return err
}

func cmdConsole(c *client.Client, args []string) error {
	fs := flag.NewFlagSet("console", flag.ContinueOnError)
	tail := fs.Int("tail", 16<<10, "bytes from the end of the log")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	id, err := one(pos, "console ID [--tail BYTES]")
	if err != nil {
		return err
	}
	out, err := c.Console(id, *tail)
	if err != nil {
		return err
	}
	os.Stdout.Write(out)
	return nil
}

// cmdExec types a command on the guest's serial console and prints what
// appears between it and an end marker. BETA: no exit code, no stdin,
// output interleaves with kernel messages; replaced by a vsock agent later.
func cmdExec(c *client.Client, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: dune exec ID CMD...")
	}
	id, cmd := args[0], strings.Join(args[1:], " ")
	b := make([]byte, 4)
	rand.Read(b)
	tag := hex.EncodeToString(b)
	ready, begin, end := "DUNE-READY-"+tag, "DUNE-BEGIN-"+tag, "DUNE-END-"+tag
	// Quotes split the markers so the echoed command line never matches.
	// Input sent before the guest shell is up is lost, so first probe with a
	// harmless echo until the shell answers, then send the command once.
	probe := fmt.Sprintf("echo DUNE-READY''-%s\n", tag)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			return errors.New("guest shell not responding on the serial console (see dune console)")
		}
		if err := c.SendConsole(id, []byte(probe)); err != nil {
			return err
		}
		ok, err := waitConsole(c, id, ready, time.Second)
		if err != nil {
			return err
		}
		if ok {
			break
		}
	}
	line := fmt.Sprintf("echo DUNE-BEGIN''-%s; %s; echo DUNE-END''-%s\n", tag, cmd, tag)
	if err := c.SendConsole(id, []byte(line)); err != nil {
		return err
	}
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.Console(id, 256<<10)
		if err != nil {
			return err
		}
		i := bytes.LastIndex(out, []byte(begin+"\r\n"))
		j := bytes.LastIndex(out, []byte(end))
		if i >= 0 && j > i {
			os.Stdout.Write(bytes.ReplaceAll(out[i+len(begin)+2:j], []byte("\r\n"), []byte("\n")))
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timed out waiting for command output (see dune console)")
}

// waitConsole polls the console log for want until timeout.
func waitConsole(c *client.Client, id, want string, timeout time.Duration) (bool, error) {
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		out, err := c.Console(id, 64<<10)
		if err != nil {
			return false, err
		}
		if bytes.Contains(out, []byte(want)) {
			return true, nil
		}
	}
	return false, nil
}

// cmdSnapLs prints the snapshot forest: each snapshot under its parent, and
// the VMs forked from it.
func cmdSnapLs(c *client.Client) error {
	sns, err := c.Snapshots()
	if err != nil {
		return err
	}
	vms, err := c.VMs()
	if err != nil {
		return err
	}
	children := map[string][]model.Snapshot{}
	known := map[string]bool{}
	for _, sn := range sns {
		known[sn.ID] = true
	}
	for _, sn := range sns {
		p := sn.Parent
		if !known[p] {
			p = ""
		}
		children[p] = append(children[p], sn)
	}
	forks := map[string][]string{}
	for _, vm := range vms {
		if vm.Origin != "" {
			forks[vm.Origin] = append(forks[vm.Origin], vm.ID)
		}
	}
	var walk func(parent, indent string)
	walk = func(parent, indent string) {
		list := children[parent]
		sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
		for _, sn := range list {
			fmt.Printf("%s%s  %-4s  vm %s  %s", indent, sn.ID, sn.Kind, sn.VMID, age(sn.CreatedAt))
			if f := forks[sn.ID]; len(f) > 0 {
				fmt.Printf("  forks: %s", strings.Join(f, " "))
			}
			fmt.Println()
			walk(sn.ID, indent+"  ")
		}
	}
	walk("", "")
	return nil
}

func cmdImageLs(c *client.Client) error {
	ims, err := c.Images()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tAGE")
	for _, im := range ims {
		fmt.Fprintf(w, "%s\t%s\n", im.Name, age(im.CreatedAt))
	}
	return w.Flush()
}

func cmdImageImport(c *client.Client, args []string) error {
	fs := flag.NewFlagSet("image import", flag.ContinueOnError)
	kernel := fs.String("kernel", "", "kernel (vmlinux) path on the daemon host")
	rootfs := fs.String("rootfs", "", "ext4 rootfs path on the daemon host")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	name, err := one(pos, "image import NAME --kernel PATH --rootfs PATH")
	if err != nil {
		return err
	}
	im, err := c.ImportImage(model.ImportImageRequest{Name: name, Kernel: *kernel, Rootfs: *rootfs})
	if err != nil {
		return err
	}
	fmt.Println(im.Name)
	return nil
}

func cmdStorage(c *client.Client, args []string) error {
	get := c.Storage
	if len(args) == 1 && args[0] == "trim" {
		get = c.Trim
	} else if len(args) > 0 {
		return errors.New("usage: dune storage [trim]")
	}
	u, err := get()
	if err != nil {
		return err
	}
	fmt.Printf("mounted:    %v\nfs used:    %s of %s\non host:    %s (sparse image)\n",
		u.Mounted, human(u.FSUsed), human(u.FSTotal), human(u.ImageAllocated))
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
