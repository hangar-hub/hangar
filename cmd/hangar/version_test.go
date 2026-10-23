package main

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFromBuildInfo checks where hangar version gets its values: -ldflags
// for release builds, and the build information Go embeds for builds without
// them, such as go install of a tagged version.
func TestFromBuildInfo(t *testing.T) {
	vcs := []debug.BuildSetting{
		{Key: "-trimpath", Value: "true"},
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "1d42e93ab2c3d4e5f60718293a4b5c6d7e8f9012"},
		{Key: "vcs.time", Value: "2026-10-21T09:53:28Z"},
		{Key: "vcs.modified", Value: "false"},
	}
	for name, tc := range map[string]struct {
		v, c, d string
		info    *debug.BuildInfo
		want    [3]string
	}{
		"no build information": {
			v: "dev", c: "none", d: "unknown",
			want: [3]string{"dev", "none", "unknown"},
		},
		"go install of a tagged version": {
			v: "dev", c: "none", d: "unknown",
			info: &debug.BuildInfo{Main: debug.Module{Path: "github.com/hangar-hub/hangar", Version: "v1.2.3"}},
			want: [3]string{"1.2.3", "none", "unknown"},
		},
		"go build in a git checkout": {
			v: "dev", c: "none", d: "unknown",
			info: &debug.BuildInfo{
				Main:     debug.Module{Path: "github.com/hangar-hub/hangar", Version: "v1.2.4-0.20261021095328-1d42e93ab2c3"},
				Settings: vcs,
			},
			want: [3]string{"1.2.4-0.20261021095328-1d42e93ab2c3", "1d42e93", "2026-10-21T09:53:28Z"},
		},
		"test binary or build outside a module": {
			v: "dev", c: "none", d: "unknown",
			info: &debug.BuildInfo{Main: debug.Module{Path: "github.com/hangar-hub/hangar", Version: "(devel)"}},
			want: [3]string{"dev", "none", "unknown"},
		},
		"ldflags win": {
			v: "1.2.3", c: "abcdef0", d: "2026-10-21T10:00:00Z",
			info: &debug.BuildInfo{
				Main:     debug.Module{Path: "github.com/hangar-hub/hangar", Version: "v9.9.9"},
				Settings: vcs,
			},
			want: [3]string{"1.2.3", "abcdef0", "2026-10-21T10:00:00Z"},
		},
		"short revision": {
			v: "dev", c: "none", d: "unknown",
			info: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "1d42e"}}},
			want: [3]string{"dev", "1d42e", "unknown"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			v, c, d := fromBuildInfo(tc.v, tc.c, tc.d, tc.info)
			require.Equal(t, tc.want, [3]string{v, c, d})
		})
	}
}
