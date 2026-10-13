package rangeio_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/index/rangeio"
)

func TestReaderFetchesWindowsOnDemand(t *testing.T) {
	data := make([]byte, 10_000)
	for i := range data {
		data[i] = byte(i)
	}
	var calls [][2]int64
	r := rangeio.New(context.Background(), int64(len(data)), 1000, func(_ context.Context, start, end int64) (io.ReadCloser, error) {
		calls = append(calls, [2]int64{start, end})
		return io.NopCloser(bytes.NewReader(data[start : end+1])), nil
	})
	require.Equal(t, int64(10_000), r.Size())
	buf := make([]byte, 10)
	n, err := r.ReadAt(buf, 995)
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.Equal(t, data[995:1005], buf)
	require.Equal(t, [][2]int64{{0, 999}, {1000, 1999}}, calls)
	_, err = r.ReadAt(buf, 1500)
	require.NoError(t, err)
	require.Len(t, calls, 2, "cached windows are reused")
	for _, off := range []int64{3000, 4000, 5000, 6000} {
		_, err = r.ReadAt(buf[:1], off)
		require.NoError(t, err)
	}
	_, err = r.ReadAt(buf[:1], 0)
	require.NoError(t, err)
	require.Len(t, calls, 7, "only the most recent windows are kept")
	require.Equal(t, int64(7000), r.Fetched)

	n, err = r.ReadAt(buf, 9995)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 5, n)
	_, err = r.ReadAt(buf, 10_000)
	require.ErrorIs(t, err, io.EOF)
	n, err = r.ReadAt(nil, 20_000)
	require.NoError(t, err)
	require.Zero(t, n)
	_, err = r.ReadAt(buf, -1)
	require.Error(t, err)
}

func TestReaderErrors(t *testing.T) {
	boom := errors.New("boom")
	r := rangeio.New(context.Background(), 100, 0, func(context.Context, int64, int64) (io.ReadCloser, error) { return nil, boom })
	_, err := r.ReadAt(make([]byte, 1), 0)
	require.ErrorIs(t, err, boom)

	short := rangeio.New(context.Background(), 100, 0, func(context.Context, int64, int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("abc"))), nil
	})
	_, err = short.ReadAt(make([]byte, 1), 0)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := rangeio.New(ctx, 100, 0, func(context.Context, int64, int64) (io.ReadCloser, error) { return nil, nil })
	_, err = cancelled.ReadAt(make([]byte, 1), 0)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCursor(t *testing.T) {
	c := &rangeio.Cursor{R: rangeio.FromBytes([]byte("hello world"))}
	buf := make([]byte, 5)
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf[:n]))
	c.Skip(1)
	b, err := io.ReadAll(c)
	require.NoError(t, err)
	require.Equal(t, "world", string(b))
}
