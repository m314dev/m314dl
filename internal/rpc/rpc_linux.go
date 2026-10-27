package rpc

import (
	"os/exec"
	"syscall"
)

// hardenCmd sets up a job's child process for a server that runs unattended:
//   - Pdeathsig SIGKILL: if the RPC server dies (even by SIGKILL), the kernel
//     kills the child too, so a crash never leaves orphaned downloads running.
//   - Setpgid: the child leads its own process group, so anything it starts
//     (for example ffmpeg) stays in that group and can be signalled as a unit.
func hardenCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
}
