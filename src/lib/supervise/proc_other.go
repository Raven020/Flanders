//go:build !unix

package supervise

import "os/exec"

// setpgid is a no-op off Unix: process groups are not available the same way, so
// the kill below targets the direct process only.
func setpgid(*exec.Cmd) {}

// killGroup kills the direct process. (Flanders targets Unix-like dev machines;
// this fallback only keeps `go build` green on other platforms.)
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
