package card_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/index/card"
)

func TestSplitFindsFrontMatter(t *testing.T) {
	front, body, ok := card.Split([]byte("\xef\xbb\xbf---\r\nlicense: mit\r\n---\r\n# Title\n"))
	require.True(t, ok)
	require.Equal(t, "license: mit\r\n", string(front))
	require.Equal(t, "# Title\n", string(body))

	front, body, ok = card.Split([]byte("---\n---\n"))
	require.True(t, ok)
	require.Empty(t, front)
	require.Empty(t, body)

	front, _, ok = card.Split([]byte("---\na: 1\n---"))
	require.True(t, ok)
	require.Equal(t, "a: 1\n", string(front))

	for _, s := range []string{"# no front matter", "---\nnever closed\n", "--- \nx\n---\n"} {
		_, body, ok := card.Split([]byte(s))
		require.False(t, ok, s)
		require.Equal(t, s, string(body))
	}
}

func TestParseGivesJSONTypes(t *testing.T) {
	data, err := card.FromReadme([]byte(`---
license: apache-2.0
tags: [a, b]
datasets:
  - one
model-index:
  - name: x
    results: []
count: 3
ratio: 0.5
inf: .inf
flag: yes
real: true
none: ~
date: 2024-01-02
octal: 0o17
big: 123456789012345678901234567890
---
body`))
	require.NoError(t, err)
	require.Equal(t, "apache-2.0", data["license"])
	require.Equal(t, []any{"a", "b"}, data["tags"])
	require.Equal(t, int64(3), data["count"])
	require.InDelta(t, 0.5, data["ratio"], 0)
	require.Equal(t, ".inf", data["inf"])
	require.Equal(t, "yes", data["flag"])
	require.Equal(t, true, data["real"])
	require.Nil(t, data["none"])
	require.Equal(t, "2024-01-02", data["date"])
	require.Equal(t, int64(15), data["octal"])
	require.InDelta(t, 1.2345678901234568e+29, data["big"], 1e15)
	require.Equal(t, []string{"one"}, card.Strings(data, "datasets"))
	require.Equal(t, "apache-2.0", card.String(data, "license"))
	require.Empty(t, card.String(data, "missing"))
	require.Nil(t, card.Strings(map[string]any{"x": ""}, "x"))
	require.Nil(t, card.Strings(map[string]any{"x": 3}, "x"))

	none, err := card.FromReadme([]byte("# Title only"))
	require.NoError(t, err)
	require.Nil(t, none)
	empty, err := card.Parse(nil)
	require.NoError(t, err)
	require.Empty(t, empty)
	null, err := card.Parse([]byte("~"))
	require.NoError(t, err)
	require.Empty(t, null)
}

func TestParseRefusesUnsafeYAML(t *testing.T) {
	cases := map[string]string{
		"alias":       "a: &x 1\nb: *x\n",
		"merge":       "base: {a: 1}\nother:\n  <<: {b: 2}\n",
		"custom tag":  "a: !python/object foo\n",
		"not a map":   "- a\n- b\n",
		"invalid":     "a: [\n",
		"complex key": "? [a, b]\n: c\n",
		"too deep":    strings.Repeat("[", 70) + strings.Repeat("]", 70),
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := card.Parse([]byte(y))
			require.ErrorIs(t, err, card.ErrInvalid)
		})
	}
	_, err := card.Parse([]byte("a: '" + strings.Repeat("x", card.MaxFrontMatter) + "'"))
	require.ErrorIs(t, err, card.ErrInvalid)
}

func FuzzFromReadme(f *testing.F) {
	f.Add([]byte("---\nlicense: mit\ntags: [a]\n---\n# x"))
	f.Add([]byte("---\na: &x [1]\nb: *x\n---\n"))
	f.Add([]byte("no front matter"))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := card.FromReadme(data)
		if err != nil {
			require.ErrorIs(t, err, card.ErrInvalid)
			return
		}
		_ = card.Strings(m, "tags")
	})
}
