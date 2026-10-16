//go:build compat && unix

package ollama_test

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serveStopTimeout is how long `ollama serve` has to stop its model
// runners and exit after SIGTERM before the test kills its process group.
const serveStopTimeout = 15 * time.Second

// prepareServe starts `ollama serve` as the leader of a new process group.
// The llama-server runners that serve starts inherit that group, so
// stopServe can reach them even after serve itself has exited.
func prepareServe(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = serveStopTimeout
}

// stopServe stops `ollama serve` and every runner it started. It sends
// SIGTERM, which makes serve unload its models and stop their runners,
// then sends SIGKILL to the whole process group, so a runner that outlived
// serve does not stay running after its model directory under home is
// deleted. It fails the test if any process that references home is still
// running afterwards, and kills it.
func stopServe(t *testing.T, cmd *exec.Cmd, home string) {
	t.Helper()
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(serveStopTimeout):
		t.Logf("ollama serve did not exit within %s of SIGTERM; killing its process group", serveStopTimeout)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("kill the ollama serve process group %d: %v", pgid, err)
	}

	var left []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		left, err = processesReferencing(home)
		if err != nil {
			t.Errorf("list processes: %v", err)
			return
		}
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(left) == 0 {
		return
	}
	t.Errorf("processes started by ollama serve are still running after it stopped:\n%s", strings.Join(left, "\n"))
	for _, line := range left {
		if pid, err := strconv.Atoi(strings.Fields(line)[0]); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// processesReferencing returns a "pid command" line for each running
// process whose command line contains dir.
func processesReferencing(dir string) ([]string, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" && strings.Contains(line, dir) {
			found = append(found, line)
		}
	}
	return found, nil
}
