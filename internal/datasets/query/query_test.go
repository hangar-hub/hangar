package query_test

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/query"
)

var cols = []query.Column{
	{Name: "id", Type: features.Primitive(features.Int64)},
	{Name: "name", Type: features.Primitive(features.String)},
	{Name: "score", Type: features.Primitive(features.Float64)},
	{Name: "ok", Type: features.Primitive(features.Bool)},
	{Name: "when", Type: &features.Arrow{ID: features.Timestamp, Unit: "s"}},
	{Name: "meta", Type: features.StructOf(features.ArrowField{Name: "lang", Type: features.Primitive(features.String)})},
	{Name: "tags", Type: features.ListOf(features.Primitive(features.String))},
	{Name: "big", Type: features.Primitive(features.Uint64)},
	{Name: "a b \"c\"", Type: features.Primitive(features.Int64)},
}

var rows = []map[string]any{
	{"id": int64(1), "name": "Alice", "score": 2.5, "ok": true, "when": time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), "meta": []any{"en"}, "big": uint64(1 << 63), "a b \"c\"": int64(1)},
	{"id": int64(2), "name": "bob", "score": nil, "ok": false, "when": time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC), "meta": []any{"fr"}, "big": uint64(5), "a b \"c\"": int64(2)},
	{"id": int64(3), "name": nil, "score": -1.0, "ok": nil, "when": nil, "meta": nil, "big": nil, "a b \"c\"": nil},
	{"id": int64(4), "name": "Al_x%", "score": 10.0, "ok": true, "when": time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), "meta": []any{nil}, "big": uint64(7), "a b \"c\"": int64(4)},
}

func run(t *testing.T, where, orderby string) []int64 {
	t.Helper()
	w, err := query.ParseWhere(where)
	require.NoError(t, err, where)
	o, err := query.ParseOrderBy(orderby)
	require.NoError(t, err, orderby)
	p, err := query.Compile(w, o, cols)
	require.NoError(t, err)
	type hit struct {
		id  int64
		key []any
	}
	var hits []hit
	for _, r := range rows {
		vals := make([]any, len(p.Columns))
		for i, c := range p.Columns {
			vals[i] = r[c]
		}
		if p.Match(vals) {
			hits = append(hits, hit{r["id"].(int64), p.Key(vals)})
		}
	}
	if p.Ordered() {
		sort.SliceStable(hits, func(i, j int) bool { return p.Less(hits[i].key, hits[j].key) })
	}
	out := []int64{}
	for _, h := range hits {
		out = append(out, h.id)
	}
	return out
}

func TestWhere(t *testing.T) {
	cases := map[string][]int64{
		``:                               {1, 2, 3, 4},
		`"id" = 2`:                       {2},
		`"id" == 2`:                      {2},
		`"id" <> 2`:                      {1, 3, 4},
		`"id" != 2`:                      {1, 3, 4},
		`"id" >= 2 AND "id" < 4`:         {2, 3},
		`"id" <= 1 OR "id" > 3`:          {1, 4},
		`NOT "id" = 1`:                   {2, 3, 4},
		`NOT ("id" = 1 OR "id" = 2)`:     {3, 4},
		`"score" > 0`:                    {1, 4},
		`"score" > '0'`:                  {1, 4},
		`"score" IS NULL`:                {2},
		`"score" IS NOT NULL`:            {1, 3, 4},
		`NOT "score" > 0`:                {3},
		`"name" = 'bob'`:                 {2},
		`"name" > 'B'`:                   {2},
		`"name" LIKE 'Al%'`:              {1, 4},
		`"name" LIKE 'Al\_x%'`:           {},
		`"name" LIKE 'Al_x\%'`:           {},
		`"name" LIKE 'Al_x%%'`:           {4},
		`"name" NOT LIKE 'Al%'`:          {2},
		`"name" ILIKE 'BOB'`:             {2},
		`"name" IN ('bob', 'Alice')`:     {1, 2},
		`"name" NOT IN ('bob', NULL)`:    {},
		`"id" IN (1, 3)`:                 {1, 3},
		`"ok" = TRUE`:                    {1, 4},
		`"ok" = 'false'`:                 {2},
		`"ok" = 0`:                       {2},
		`"when" >= '2021-01-01'`:         {2, 4},
		`"when" < '2021-06-01 12:00:01'`: {1, 2},
		`"meta"."lang" = 'fr'`:           {2},
		`"meta"."lang" IS NULL`:          {3, 4},
		`"big" > 6`:                      {1, 4},
		`"big" = 9223372036854775808`:    {1},
		`"id" = 2.0`:                     {2},
		`"id" < 2.5`:                     {1, 2},
		`"id" = NULL`:                    {},
		`("id" = 1) OR ("name" = 'bob' AND "ok" = FALSE)`: {1, 2},
		`"a b ""c""" = 4`: {4},
		`"score" > -2e0`:  {1, 3, 4},
		`"score" < .5`:    {3},
	}
	for where, want := range cases {
		require.Equal(t, want, run(t, where, ""), where)
	}
}

func TestOrderBy(t *testing.T) {
	require.Equal(t, []int64{4, 1, 3, 2}, run(t, "", `"score" DESC`))
	require.Equal(t, []int64{3, 1, 4, 2}, run(t, "", `"score" ASC`))
	require.Equal(t, []int64{4, 1, 2, 3}, run(t, "", `"name"`), "bytewise: _ sorts before i")
	require.Equal(t, []int64{2, 4, 1, 3}, run(t, "", `"ok", "id" DESC`))
	require.Equal(t, []int64{4, 2, 1, 3}, run(t, "", `"when" DESC`))
	require.Equal(t, []int64{4, 1}, run(t, `"ok" = TRUE`, `"id" desc`))
}

func TestErrors(t *testing.T) {
	syntax := []string{`id = 1`, `"id" = `, `"id" 1`, `"id" = 1 AND`, `("id" = 1`, `"id" = 1)`, `"id" IS 1`, `"id" IN 1`,
		`"id" IN (1`, `"name" LIKE 1`, `"id" = 'x`, `"id" = 1; DROP TABLE x`, `""= 1`, `"id" ! 1`, `"id" = 1 --`, `"id".`,
		`"id" = -`, `"id" = é`, "\xff"}
	for _, s := range syntax {
		_, err := query.ParseWhere(s)
		require.ErrorIs(t, err, query.ErrSyntax, s)
	}
	for _, s := range []string{`id`, `"id" UP`, `"id",`, `"id" "name"`} {
		_, err := query.ParseOrderBy(s)
		require.ErrorIs(t, err, query.ErrSyntax, s)
	}
	types := []string{`"nope" = 1`, `"id" = 'x'`, `"tags" = 'x'`, `"ok" = 'maybe'`, `"when" = 5`, `"when" = 'soon'`,
		`"id"."x" = 1`, `"meta"."nope" = 1`, `"meta" = 1`, `"tags" LIKE 'x'`}
	for _, s := range types {
		w, err := query.ParseWhere(s)
		require.NoError(t, err, s)
		_, err = query.Compile(w, nil, cols)
		require.ErrorIs(t, err, query.ErrType, s)
	}
	o, err := query.ParseOrderBy(`"tags"`)
	require.NoError(t, err)
	_, err = query.Compile(nil, o, cols)
	require.ErrorIs(t, err, query.ErrType)
	long := make([]byte, query.MaxLength+1)
	for i := range long {
		long[i] = ' '
	}
	_, err = query.ParseWhere(`"id" = 1` + string(long))
	require.ErrorIs(t, err, query.ErrSyntax)
	deep := ""
	for range 100 {
		deep += "("
	}
	_, err = query.ParseWhere(deep + `"id" = 1`)
	require.ErrorIs(t, err, query.ErrSyntax)
}

func TestStringRoundTrip(t *testing.T) {
	w, err := query.ParseWhere(`NOT ("a" = 'it''s' OR "b"."c" IN (1, TRUE, NULL)) AND "d" IS NOT NULL AND "e" NOT ILIKE '%x'`)
	require.NoError(t, err)
	require.Equal(t, `((NOT ("a" = 'it''s' OR "b"."c" IN (1, TRUE, NULL))) AND "d" IS NOT NULL) AND "e" NOT ILIKE '%x'`, w.String()[1:len(w.String())-1])
	again, err := query.ParseWhere(w.String())
	require.NoError(t, err)
	require.Equal(t, w.String(), again.String())
}

func TestMayMatch(t *testing.T) {
	w, err := query.ParseWhere(`"id" > 10 AND ("score" < 0 OR "score" = 5)`)
	require.NoError(t, err)
	p, err := query.Compile(w, nil, cols)
	require.NoError(t, err)
	require.False(t, p.MayMatch(map[string][2]any{"id": {int64(0), int64(5)}}))
	require.True(t, p.MayMatch(map[string][2]any{"id": {int64(0), int64(50)}}))
	require.False(t, p.MayMatch(map[string][2]any{"id": {int64(0), int64(50)}, "score": {1.0, 4.0}}))
	require.True(t, p.MayMatch(map[string][2]any{"id": {int64(0), int64(50)}, "score": {1.0, 5.0}}))
	require.True(t, p.MayMatch(nil))
	w, err = query.ParseWhere(`"name" = 'm' OR "ok" = TRUE`)
	require.NoError(t, err)
	p, err = query.Compile(w, nil, cols)
	require.NoError(t, err)
	require.True(t, p.MayMatch(map[string][2]any{"name": {"x", "z"}}), "the bool comparison cannot prune")
	empty, err := query.Compile(nil, nil, cols)
	require.NoError(t, err)
	require.True(t, empty.MayMatch(nil))
	require.True(t, empty.Match(nil))
	require.False(t, empty.Ordered())
}

// FuzzWhere checks that any input either fails to parse or parses to a
// clause whose printed form parses to the same clause, and compiles or
// fails without panicking.
func FuzzWhere(f *testing.F) {
	for _, s := range []string{`"id" = 1`, `NOT ("a" IN (1, 'x', NULL) OR "b" LIKE '%q_')`, `"x"."y" IS NOT NULL AND "z" <= -1.5e3`,
		`"name" ILIKE 'a' OR "ok" = TRUE`, `(("id" > 1))`, `"a""b" != 'c''d'`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		w, err := query.ParseWhere(s)
		if err != nil || w == nil {
			return
		}
		again, err := query.ParseWhere(w.String())
		if err != nil {
			t.Fatalf("%q printed as %q, which does not parse: %v", s, w.String(), err)
		}
		if again.String() != w.String() {
			t.Fatalf("%q printed as %q, then as %q", s, w.String(), again.String())
		}
		if p, err := query.Compile(w, nil, cols); err == nil {
			vals := make([]any, len(p.Columns))
			_ = p.Match(vals)
			for i, c := range p.Columns {
				vals[i] = rows[0][c]
			}
			_ = p.Match(vals)
		}
	})
}

// FuzzOrderBy checks the orderby parser on any input.
func FuzzOrderBy(f *testing.F) {
	for _, s := range []string{`"id"`, `"a" DESC, "b" asc`, `"x"."y"`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		o, err := query.ParseOrderBy(s)
		if err != nil {
			return
		}
		if p, err := query.Compile(nil, o, cols); err == nil {
			a := make([]any, len(p.Columns))
			for i, c := range p.Columns {
				a[i] = rows[1][c]
			}
			_ = p.Less(p.Key(a), p.Key(a))
		}
	})
}
