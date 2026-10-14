package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Git runs the git binary with a minimal, user-independent environment: no
// system or global configuration, an empty HOME and the C locale, so that no
// local setting (commit signing, hooks, aliases) changes what Hangar writes.
type Git struct {
	// Home is an empty directory used as HOME.
	Home string
	// Timeout bounds each command. Zero means no limit beyond the context.
	Timeout time.Duration
}

// Env returns the environment for a git subprocess, plus extra variables.
func (g Git) Env(extra ...string) []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"HOME=" + g.Home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=C",
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_REPLACE_OBJECTS=1",
	}
	return append(env, extra...)
}

// Error is a failed git command. Stderr holds git's message.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *Error) Unwrap() error { return e.Err }

// Command returns an exec.Cmd for git in dir, with the minimal environment.
// The command is killed when ctx ends; callers that want the Timeout must
// derive ctx with WithTimeout.
func (g Git) Command(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = g.Env(env...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// WithTimeout derives a context bounded by the configured timeout.
func (g Git) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if g.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, g.Timeout)
}

// Run runs git in dir with stdin and returns stdout. A non-zero exit gives an
// *Error with stderr.
func (g Git) Run(ctx context.Context, dir string, stdin io.Reader, env []string, args ...string) ([]byte, error) {
	ctx, cancel := g.WithTimeout(ctx)
	defer cancel()
	cmd := g.Command(ctx, dir, env, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errors.Join(err, ctxErr)
		}
		return stdout.Bytes(), &Error{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return stdout.Bytes(), nil
}

// Output runs git and returns stdout with surrounding whitespace trimmed.
func (g Git) Output(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := g.Run(ctx, dir, nil, nil, args...)
	return strings.TrimSpace(string(b)), err
}

// Stream runs git with stdout sent to w, without buffering it.
func (g Git) Stream(ctx context.Context, dir string, stdin io.Reader, w io.Writer, env []string, args ...string) error {
	ctx, cancel := g.WithTimeout(ctx)
	defer cancel()
	cmd := g.Command(ctx, dir, env, args...)
	cmd.Stdin = stdin
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &Error{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return nil
}

// ObjectInfo is one result of `git cat-file --batch-check`.
type ObjectInfo struct {
	OID  string
	Type string
	Size int64
	// Missing is true when the object does not exist.
	Missing bool
}

// BatchCheck returns the type and size of each object, in order, with one
// git process.
func (g Git) BatchCheck(ctx context.Context, dir string, oids []string) ([]ObjectInfo, error) {
	if len(oids) == 0 {
		return nil, nil
	}
	in := strings.Join(oids, "\n") + "\n"
	out, err := g.Run(ctx, dir, strings.NewReader(in), nil, "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return nil, err
	}
	res := make([]ObjectInfo, 0, len(oids))
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for i := 0; sc.Scan(); i++ {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "missing" {
			res = append(res, ObjectInfo{OID: fields[0], Missing: true})
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected cat-file output %q", line)
		}
		var size int64
		if _, err := fmt.Sscan(fields[2], &size); err != nil {
			return nil, fmt.Errorf("unexpected cat-file size %q", line)
		}
		res = append(res, ObjectInfo{OID: fields[0], Type: fields[1], Size: size})
	}
	return res, sc.Err()
}

// BatchRead returns the content of each object in oids, calling fn once per
// object in order. It uses one git process and reads each object into
// memory, so callers must only pass small objects.
func (g Git) BatchRead(ctx context.Context, dir string, oids []string, fn func(info ObjectInfo, content []byte) error) error {
	if len(oids) == 0 {
		return nil
	}
	ctx, cancel := g.WithTimeout(ctx)
	defer cancel()
	cmd := g.Command(ctx, dir, nil, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	br := bufio.NewReader(stdout)
	readErr := func() error {
		for range oids {
			header, err := br.ReadString('\n')
			if err != nil {
				return fmt.Errorf("read cat-file header: %w", err)
			}
			fields := strings.Fields(header)
			if len(fields) == 2 && fields[1] == "missing" {
				if err := fn(ObjectInfo{OID: fields[0], Missing: true}, nil); err != nil {
					return err
				}
				continue
			}
			if len(fields) != 3 {
				return fmt.Errorf("unexpected cat-file header %q", header)
			}
			var size int64
			if _, err := fmt.Sscan(fields[2], &size); err != nil {
				return fmt.Errorf("unexpected cat-file header %q", header)
			}
			buf := make([]byte, size+1)
			if _, err := io.ReadFull(br, buf); err != nil {
				return fmt.Errorf("read cat-file body: %w", err)
			}
			if err := fn(ObjectInfo{OID: fields[0], Type: fields[1], Size: size}, buf[:size]); err != nil {
				return err
			}
		}
		return nil
	}()
	if readErr != nil {
		_ = cmd.Process.Kill()
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
		return readErr
	}
	if err := cmd.Wait(); err != nil {
		return &Error{Args: []string{"cat-file", "--batch"}, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return nil
}

// OpenBlob streams one object's content from `git cat-file blob`. Close
// waits for the process.
func (g Git) OpenBlob(ctx context.Context, dir, oid string) (io.ReadCloser, error) {
	ctx, cancel := g.WithTimeout(ctx)
	cmd := g.Command(ctx, dir, nil, "cat-file", "blob", oid)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return &cmdReader{r: stdout, cmd: cmd, cancel: cancel, stderr: &stderr, args: []string{"cat-file", "blob", oid}}, nil
}

type cmdReader struct {
	r      io.Reader
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stderr *bytes.Buffer
	args   []string
	done   bool
}

func (c *cmdReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) && !c.done {
		c.done = true
		if werr := c.cmd.Wait(); werr != nil {
			c.cancel()
			return n, &Error{Args: c.args, Stderr: strings.TrimSpace(c.stderr.String()), Err: werr}
		}
		c.cancel()
	}
	return n, err
}

func (c *cmdReader) Close() error {
	if c.done {
		return nil
	}
	c.done = true
	c.cancel()
	_ = c.cmd.Wait()
	return nil
}
