package cli

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/hangar-hub/hangar/internal/api/gen"
)

// pollInterval is how often the CLI asks for Job progress. Tests shorten it.
var pollInterval = time.Second

func (a *app) jobsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List, watch and cancel your Pulls and other Jobs",
		Args:  args(cobra.NoArgs),
	}
	a.clientFlags(cmd)
	var all bool
	var kind, status string
	list := &cobra.Command{
		Use:   "list",
		Short: "List your Jobs, newest first",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd).listJobs(cmd.Context(), all, kind, status)
		},
	}
	list.Flags().BoolVar(&all, "all", false, "list every Job you can see, not only yours (admins see the whole Instance)")
	list.Flags().StringVar(&kind, "kind", "", "only Jobs of this kind, such as pull")
	list.Flags().StringVar(&status, "status", "", "only Jobs with this status: pending, running, succeeded, failed or cancelled")
	watch := &cobra.Command{
		Use:   "watch <job-id>",
		Short: "Show a Job's progress until it finishes",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			id, err := parseJobID(argv[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			j, err := a.watchJob(cmd.Context(), c, id, "Job "+argv[0])
			if a.json && j != nil {
				if perr := a.printJSON(j); perr != nil {
					return perr
				}
			}
			return err
		},
	}
	cancel := &cobra.Command{
		Use:   "cancel <job-id>",
		Short: "Cancel a Job",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			id, err := parseJobID(argv[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			r, err := c.api.CancelJobWithResponse(cmd.Context(), id)
			if err != nil {
				return c.reach(err)
			}
			if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
				return err
			}
			if a.json {
				return a.printJSON(r.JSON200)
			}
			a.printf("Cancelled Job %s.\n", id)
			return nil
		},
	}
	cmd.AddCommand(list, watch, cancel)
	return cmd
}

func parseJobID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, usagef("%q is not a Job ID", s)
	}
	return id, nil
}

func (a *app) listJobs(ctx context.Context, all bool, kind, status string) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	params := &gen.ListJobsParams{Limit: ptr(100)}
	if !all {
		params.Mine = ptr(true)
	}
	if kind != "" {
		params.Kind = &kind
	}
	if status != "" {
		s := gen.JobStatus(status)
		switch s {
		case gen.JobStatusPending, gen.JobStatusRunning, gen.JobStatusSucceeded, gen.JobStatusFailed, gen.JobStatusCancelled:
		default:
			return usagef("--status must be pending, running, succeeded, failed or cancelled")
		}
		params.Status = &s
	}
	r, err := c.api.ListJobsWithResponse(ctx, params)
	if err != nil {
		return c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(r.JSON200.Items)
	}
	if len(r.JSON200.Items) == 0 {
		a.printf("No Jobs.\n")
		return nil
	}
	rows := make([][]string, 0, len(r.JSON200.Items))
	for _, j := range r.JSON200.Items {
		rows = append(rows, []string{j.Id.String(), j.Kind, string(j.Status), jobTarget(j), progressSummary(j.Progress), j.CreatedAt.Local().Format("2006-01-02 15:04")})
	}
	return a.table([]string{"ID", "KIND", "STATUS", "TARGET", "PROGRESS", "CREATED"}, rows)
}

// jobTarget names what a Job works on, from its payload.
func jobTarget(j gen.Job) string {
	if j.Payload == nil {
		return "-"
	}
	p := *j.Payload
	for _, k := range []string{"repo", "upstream_repo", "repo_id"} {
		if s, ok := p[k].(string); ok && s != "" {
			return s
		}
	}
	return "-"
}

// progressSummary formats Job progress for a table cell.
func progressSummary(p gen.JobProgress) string {
	var parts []string
	if p.BytesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s of %s", formatBytes(p.BytesDone), formatBytes(p.BytesTotal)))
	}
	if p.FilesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d files", p.FilesDone, p.FilesTotal))
	}
	if p.Message != nil && *p.Message != "" {
		parts = append(parts, *p.Message)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// terminal reports whether a Job has finished.
func terminal(s gen.JobStatus) bool {
	return s == gen.JobStatusSucceeded || s == gen.JobStatusFailed || s == gen.JobStatusCancelled
}

// watchJob polls a Job until it finishes, printing progress to stderr. It
// returns an error if the Job failed or was cancelled.
func (a *app) watchJob(ctx context.Context, c *client, id uuid.UUID, label string) (*gen.Job, error) {
	var last string
	for {
		j, err := c.job(ctx, id)
		if err != nil {
			return nil, err
		}
		line := fmt.Sprintf("%s: %s", label, j.Status)
		if s := progressSummary(j.Progress); s != "-" {
			line += ", " + s
		}
		if j.Progress.Rate > 0 && j.Status == gen.JobStatusRunning {
			line += ", " + formatBytes(int64(j.Progress.Rate)) + "/s"
			if j.Progress.EtaSeconds != nil {
				line += ", " + formatDuration(time.Duration(*j.Progress.EtaSeconds)*time.Second) + " left"
			}
		}
		if line != last {
			a.logf("%s\n", line)
			last = line
		}
		if terminal(j.Status) {
			switch j.Status {
			case gen.JobStatusFailed:
				msg := deref(j.Error)
				for _, f := range deref(j.Progress.Files) {
					if f.Error != nil && *f.Error != "" {
						msg += fmt.Sprintf("; %s: %s", f.Path, *f.Error)
					}
				}
				return j, fmt.Errorf("%s failed: %s", label, orDash(strings.TrimPrefix(msg, "; ")))
			case gen.JobStatusCancelled:
				return j, fmt.Errorf("%s was cancelled", label)
			}
			return j, nil
		}
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
