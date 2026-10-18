package httpx_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

func TestSetProtocolLabelsHTTPMetrics(t *testing.T) {
	l := &otelhttp.Labeler{}
	ctx := otelhttp.ContextWithLabeler(context.Background(), l)
	httpx.SetProtocol(ctx, telemetry.ProtocolGit)
	require.Len(t, l.Get(), 1)
	require.Equal(t, telemetry.AttrProtocol, l.Get()[0].Key)
	require.Equal(t, string(telemetry.ProtocolGit), l.Get()[0].Value.AsString())

	// Without a labeler in the context nothing happens.
	httpx.SetProtocol(context.Background(), telemetry.ProtocolGit)
}
