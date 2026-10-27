//go:build !linux

package rpc

import "os/exec"

// hardenCmd does nothing off Linux, where Pdeathsig is not available. A
// graceful shutdown still stops the children; only the cleanup of orphans
// after a hard crash is limited to Linux.
func hardenCmd(cmd *exec.Cmd) {}
