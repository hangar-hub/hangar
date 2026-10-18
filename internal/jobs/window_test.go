package jobs_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/jobs"
)

func TestWindows(t *testing.T) {
	utc := func(h, m int) time.Time { return time.Date(2026, 10, 13, h, m, 0, 0, time.UTC) }

	none, err := jobs.ParseWindows(nil, nil)
	require.NoError(t, err)
	require.True(t, none.Any())
	require.True(t, none.Open(utc(12, 0)))
	require.Equal(t, utc(12, 0), none.NextOpen(utc(12, 0)))

	w, err := jobs.ParseWindows([]string{"01:00-07:00", "22:30-23:00"}, time.UTC)
	require.NoError(t, err)
	require.False(t, w.Any())
	require.True(t, w.Open(utc(1, 0)))
	require.True(t, w.Open(utc(6, 59)))
	require.False(t, w.Open(utc(7, 0)), "the end is exclusive")
	require.False(t, w.Open(utc(12, 0)))
	require.True(t, w.Open(utc(22, 45)))
	require.Equal(t, utc(22, 30), w.NextOpen(utc(12, 0)))
	require.Equal(t, time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC), w.NextOpen(utc(23, 30)))
	require.Equal(t, "01:00-07:00, 22:30-23:00", w.String())
	require.Equal(t, "Waiting for pull window (01:00-07:00, 22:30-23:00)", w.WaitMessage())

	overnight, err := jobs.ParseWindows([]string{"22:00-06:00"}, time.UTC)
	require.NoError(t, err)
	require.True(t, overnight.Open(utc(23, 0)))
	require.True(t, overnight.Open(utc(5, 59)))
	require.False(t, overnight.Open(utc(6, 0)))
	require.Equal(t, utc(22, 0), overnight.NextOpen(utc(6, 0)))

	allDay, err := jobs.ParseWindows([]string{"03:00-03:00"}, time.UTC)
	require.NoError(t, err)
	require.True(t, allDay.Open(utc(2, 0)))

	for _, bad := range []string{"01:00", "1-7", "25:00-07:00", "01:00-07:60"} {
		_, err := jobs.ParseWindows([]string{bad}, time.UTC)
		require.Error(t, err, bad)
	}
}

func TestWindowsInTimeZoneAcrossDaylightSaving(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	require.NoError(t, err)
	w, err := jobs.ParseWindows([]string{"01:00-07:00"}, london)
	require.NoError(t, err)
	// 13 October 2026 is BST (UTC+1): 01:00 local is 00:00 UTC.
	require.True(t, w.Open(time.Date(2026, 10, 13, 0, 30, 0, 0, time.UTC)))
	require.False(t, w.Open(time.Date(2026, 10, 13, 6, 30, 0, 0, time.UTC)))
	// Clocks go forward at 01:00 GMT on 29 March 2026: 01:00 local does not
	// exist, so the window opens at 02:00 BST, which is 01:00 UTC.
	next := w.NextOpen(time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC))
	require.Equal(t, time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), next.UTC())
	require.True(t, w.Open(next))
}
