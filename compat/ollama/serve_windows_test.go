//go:build compat && windows

package ollama_test

import (
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// serveStopTimeout bounds how long cmd.Wait waits for the output of
// `ollama serve` after the process tree is killed.
const serveStopTimeout = 15 * time.Second

// prepareServe bounds how long stopServe waits for the output of serve.
func prepareServe(cmd *exec.Cmd) {
	cmd.WaitDelay = serveStopTimeout
}

// stopServe kills `ollama serve` and every runner it started. Windows has
// no process groups to signal, so it ends the whole process tree with
// taskkill, which reaches the llama-server runners that Process.Kill
// alone would leave running.
func stopServe(t *testing.T, cmd *exec.Cmd, _ string) {
	t.Helper()
	if out, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).CombinedOutput(); err != nil {
		t.Errorf("end the ollama serve process tree: %v: %s", err, out)
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}
