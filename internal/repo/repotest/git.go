package repotest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// git runs the git binary with a minimal, user-independent environment, so
// that no global or system config (such as commit signing) changes results.
type git struct {
	home string
}

func (g git) env(extra ...string) []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"HOME=" + g.home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=C",
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
	}
	return append(env, extra...)
}

// run runs git in dir and returns stdout. stderr is included in the error.
func (g git) run(ctx context.Context, dir string, stdin io.Reader, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = g.env(env...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &gitError{args: args, stderr: strings.TrimSpace(stderr.String()), err: err}
	}
	return stdout.Bytes(), nil
}

func (g git) out(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := g.run(ctx, dir, nil, nil, args...)
	return strings.TrimSpace(string(b)), err
}

type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *gitError) Unwrap() error { return e.err }
