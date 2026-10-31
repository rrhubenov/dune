// Package run executes host commands (ip, iptables, mount, ...) and turns
// failures into errors that carry the command line and its stderr.
package run

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Cmd runs name with args and returns an error including stderr on failure.
func Cmd(name string, args ...string) error {
	_, err := Output(name, args...)
	return err
}

// Output runs name with args and returns its stdout.
func Output(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Quiet runs name with args, discarding all output, and reports only success.
// For probes like "iptables -C" where failure is an answer, not an error.
func Quiet(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}
