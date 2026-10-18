package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// printJSON writes v as indented JSON.
func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table writes rows under a header, aligned in columns.
func (a *app) table(header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(header, "\t")); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintln(tw, strings.Join(r, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// printf writes human-readable output to stdout.
func (a *app) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.stdout, format, args...)
}

// logf writes progress and notes to stderr, so stdout stays clean for --json.
func (a *app) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.stderr, format, args...)
}

// formatBytes formats a byte count with binary units, such as 1.5 GiB.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatDuration formats an ETA such as 3m20s, rounded to seconds.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Second).String()
}

// orDash returns s, or "-" when it is empty.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// isTerminal reports whether w is a terminal, so progress can redraw a line.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// progress reports bytes transferred to stderr: one redrawn line on a
// terminal, otherwise a line every few seconds.
type progress struct {
	mu      sync.Mutex
	w       io.Writer
	label   string
	total   int64
	done    int64
	start   time.Time
	last    time.Time
	tty     bool
	quiet   bool
	printed bool
}

func newProgress(w io.Writer, label string, total int64, quiet bool) *progress {
	now := time.Now()
	return &progress{w: w, label: label, total: total, start: now, tty: isTerminal(w), quiet: quiet}
}

// add records n more bytes.
func (p *progress) add(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += n
	p.render(false)
}

// render prints the current state, at most every 200 ms on a terminal and
// every 5 seconds otherwise, unless final.
func (p *progress) render(final bool) {
	if p.quiet {
		return
	}
	now := time.Now()
	every := 5 * time.Second
	if p.tty {
		every = 200 * time.Millisecond
	}
	if !final && now.Sub(p.last) < every {
		return
	}
	p.last = now
	elapsed := now.Sub(p.start).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(p.done) / elapsed
	}
	line := fmt.Sprintf("%s: %s", p.label, formatBytes(p.done))
	if p.total > 0 {
		pct := float64(p.done) * 100 / float64(p.total)
		line = fmt.Sprintf("%s: %3.0f%% %s of %s", p.label, pct, formatBytes(p.done), formatBytes(p.total))
		if rate > 0 && p.done < p.total {
			eta := time.Duration(float64(p.total-p.done)/rate) * time.Second
			line += ", " + formatBytes(int64(rate)) + "/s, " + formatDuration(eta) + " left"
		}
	}
	if p.tty {
		_, _ = fmt.Fprintf(p.w, "\r\033[K%s", line)
		p.printed = true
		if final {
			_, _ = fmt.Fprintln(p.w)
		}
		return
	}
	_, _ = fmt.Fprintln(p.w, line)
}

// finish prints the final state.
func (p *progress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.render(true)
}

// progressReader counts bytes read into a progress.
type progressReader struct {
	r io.Reader
	p *progress
}

func (pr progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	if n > 0 {
		pr.p.add(int64(n))
	}
	return n, err
}

// countingProgress counts bytes read into a progress and can take them off
// again, for a request body that may be sent more than once.
type countingProgress struct {
	r io.Reader
	p *progress
	n int64
}

func (c *countingProgress) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 {
		c.n += int64(n)
		c.p.add(int64(n))
	}
	return n, err
}

// undo takes the bytes counted so far off the progress.
func (c *countingProgress) undo() {
	if c.n != 0 {
		c.p.add(-c.n)
		c.n = 0
	}
}

// statusTick is how often a status line on a terminal redraws its timer,
// statusRepeat how often it is printed again when stderr is not a
// terminal, and statusTerminal tells whether stderr is one. Tests replace
// them.
var (
	statusTick     = time.Second
	statusRepeat   = time.Minute
	statusTerminal = isTerminal
)

// waitStatus shows msg on stderr while something slow happens that has no
// progress of its own, such as an Upstream lookup or a check of a large
// upload, and returns a function that ends it. On a terminal the line
// counts the seconds and is cleared at the end; otherwise msg is printed
// at once and again with the time taken every statusRepeat, so a log shows
// that a long wait is still going. With --json nothing is shown.
func (a *app) waitStatus(msg string) func() {
	if a.json {
		return func() {}
	}
	start := time.Now()
	tty := statusTerminal(a.stderr)
	every := statusTick
	draw := func() { a.logf("\r\033[K%s (%s)", msg, formatDuration(time.Since(start))) }
	if !tty {
		every = statusRepeat
		a.logf("%s\n", msg)
		draw = func() { a.logf("%s (%s)\n", msg, formatDuration(time.Since(start))) }
	} else {
		draw()
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				draw()
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
		if tty {
			a.logf("\r\033[K")
		}
	}
}
