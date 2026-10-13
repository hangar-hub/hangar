package datasets_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets"
)

func TestConfigSplitsAndLookup(t *testing.T) {
	c := datasets.Config{Name: "default", DataFiles: map[string][]string{
		"zeta": {"z/*"}, "test": {"t/*"}, "train": {"tr/*"}, "alpha": {"a/*"},
	}}
	require.Equal(t, []string{"train", "test", "alpha", "zeta"}, c.Splits())
	r := &datasets.Resolved{Configs: []datasets.Config{c}}
	got, ok := r.Config("default")
	require.True(t, ok)
	require.Equal(t, "default", got.Name)
	_, ok = r.Config("missing")
	require.False(t, ok)
}
