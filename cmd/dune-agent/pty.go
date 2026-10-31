//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/rrhubenov/dune/internal/agent"
)

// handlePTY upgrades the connection and runs a login shell on a new pseudo
// terminal, relaying it as frames (see internal/agent/pty.go). The shell gets
// SIGHUP when the host side goes away.
func handlePTY(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), agent.PTYUpgrade) {
		http.Error(w, "expected Upgrade: "+agent.PTYUpgrade, http.StatusBadRequest)
		return
	}
	cols, rows := dim(r.URL.Query().Get("cols"), 80), dim(r.URL.Query().Get("rows"), 24)
	master, slave, err := openPTY()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSize(master, cols, rows)

	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root", "USER=root", "LOGNAME=root", "SHELL=" + shell,
		"TERM=xterm-256color", "LANG=C.UTF-8",
	}
	cmd.Dir = "/root"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// New session with the pty as controlling terminal: job control works
	// and the whole session gets SIGHUP on hangup.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err = cmd.Start()
	slave.Close()
	if err != nil {
		master.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		cmd.Process.Kill()
		master.Close()
		return
	}
	defer conn.Close()
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + agent.PTYUpgrade + "\r\n\r\n")
	brw.Flush()

	var wmu sync.Mutex
	send := func(typ byte, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return agent.WriteFrame(conn, typ, payload)
	}

	pumped := make(chan struct{})
	go func() { // terminal output -> host
		defer close(pumped)
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 && send(agent.FrameData, buf[:n]) != nil {
				return
			}
			if err != nil { // EIO once every holder of the slave side is gone
				return
			}
		}
	}()
	go func() { // host -> terminal input and resizes
		for {
			typ, payload, err := agent.ReadFrame(brw.Reader)
			if err != nil {
				unix.Kill(-cmd.Process.Pid, unix.SIGHUP)
				master.Close()
				return
			}
			switch typ {
			case agent.FrameData:
				master.Write(payload)
			case agent.FrameResize:
				if len(payload) == 4 {
					setSize(master, binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:]))
				}
			}
		}
	}()

	cmd.Wait()
	// Let the pump forward the shell's last output. A background process
	// still holding the terminal would keep it open forever, so cap the wait.
	select {
	case <-pumped:
	case <-time.After(200 * time.Millisecond):
	}
	master.Close()
	<-pumped
	code := cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		code = 128 + int(ws.Signal())
	}
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(int32(code)))
	send(agent.FrameExit, b)
}

func dim(s string, def uint16) uint16 {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return def
	}
	return uint16(n)
}

// openPTY allocates a pseudo terminal pair. The master stays non-blocking
// (pollable), so Close interrupts a pending Read.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var n int
	var ioErr error
	cerr := ctl(master, func(fd int) {
		if ioErr = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); ioErr != nil { // unlockpt
			return
		}
		n, ioErr = unix.IoctlGetInt(fd, unix.TIOCGPTN) // ptsname
	})
	if err = firstErr(cerr, ioErr); err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func setSize(master *os.File, cols, rows uint16) {
	ctl(master, func(fd int) {
		unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	})
}

// ctl runs f on the file's descriptor without switching it to blocking mode
// (which File.Fd would do).
func ctl(f *os.File, fn func(fd int)) error {
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	return sc.Control(func(fd uintptr) { fn(int(fd)) })
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
