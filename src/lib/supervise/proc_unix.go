//go:build unix

package supervise

import (
	"os/exec"
	"syscall"
)

// setpgid puts the child in its own process group, so a group-targeted kill
// reaches any subprocess the CLI itself spawned (not just the direct child).
func setpgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup SIGKILLs the whole process group (negative pid). The hard backstop is
// deliberately decisive — a timed-out or runaway loop must die, including any tool
// subprocesses it launched (specs/01 §Guardrails). Falls back to killing just the
// direct process if the group id can't be resolved (e.g. it already exited).
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
			return nil
		}
	}
	return cmd.Process.Kill()
}
