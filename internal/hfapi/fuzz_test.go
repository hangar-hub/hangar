package hfapi

// Fuzz targets for the Hub API's parsers (13-testing.md, "Fuzz"): NDJSON
// commit payloads and Model card front matter. `go test` runs the seed
// corpus; scripts/fuzz.sh runs each target with -fuzz.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/repo"
)

// commitSeeds are NDJSON commit bodies in the shapes huggingface_hub sends
// (contract §7.7), plus broken ones.
var commitSeeds = []string{
	`{"key":"header","value":{"summary":"Upload model","description":"","parentCommit":"607a30d7"}}` + "\n" +
		`{"key":"file","value":{"content":"eyJhIjogMX0=","path":"config.json","encoding":"base64"}}` + "\n" +
		`{"key":"lfsFile","value":{"path":"model.safetensors","algo":"sha256","oid":"` + strings.Repeat("a", 64) + `","size":5}}` + "\n" +
		`{"key":"lfsFile","value":{"path":"copy.safetensors","algo":"sha256","oid":"` + strings.Repeat("A", 64) + `"}}` + "\n" +
		`{"key":"deletedFile","value":{"path":"old.bin"}}` + "\n" +
		`{"key":"deletedFolder","value":{"path":"logs/"}}` + "\n",
	`{"key":"header","value":{"summary":"s"}}`,
	`{"key":"header","value":{"summary":"s"}}` + "\r\n\r\n" + `{"key":"file","value":{"path":"a","content":"plain"}}`,
	`{"key":"file","value":{"path":"a","content":""}}`,
	`{"key":"header","value":{"summary":"  "}}`,
	`{"key":"header","value":{"summary":"s"}}` + "\n" + `{"key":"header","value":{"summary":"s"}}`,
	`{"key":"header","value":{"summary":"s"}}` + "\n" + `{"key":"file","value":{"path":"a","content":"!!","encoding":"base64"}}`,
	`{"key":"header","value":{"summary":"s"}}` + "\n" + `{"key":"file","value":{"path":"a","content":"x","encoding":"latin-1"}}`,
	`{"key":"header","value":{"summary":"s"}}` + "\n" + `{"key":"lfsFile","value":{"path":"a","algo":"md5","oid":"x"}}`,
	`{"key":"header","value":{"summary":"s"}}` + "\n" + `{"key":"copy","value":{}}`,
	`{"key":"header","value":"nope"}`,
	`{"key":"header"`,
	"",
	"\n\n",
}

// fuzzHandler is a Handler with just enough settings for the parsers.
func fuzzHandler(lineMax int64) *Handler {
	h := testHandler()
	h.d.Settings.CommitLineMax = lineMax
	h.d.Settings.BodyMax = 1 << 20
	return h
}

// lines splits data after each newline, as readLine does.
func lines(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			out = append(out, data)
			break
		}
		out = append(out, data[:i+1])
		data = data[i+1:]
	}
	return out
}

// FuzzReadCommit checks that any commit body either parses into valid
// operations or fails with a client error, never a panic or a server error,
// and that the line cap holds.
func FuzzReadCommit(f *testing.F) {
	for _, s := range commitSeeds {
		f.Add([]byte(s), uint16(4096))
		f.Add([]byte(s), uint16(40))
	}
	f.Fuzz(func(t *testing.T, data []byte, lineMax uint16) {
		limit := int64(lineMax)
		c, err := fuzzHandler(limit).readCommit(bytes.NewReader(data))
		longest := 0
		for _, l := range lines(data) {
			longest = max(longest, len(l))
		}
		if err != nil {
			var ce *commitError
			require.True(t, errors.As(err, &ce), "a malformed body is a client error, not %v", err)
			require.Contains(t, []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge}, ce.status)
			require.NotEmpty(t, ce.msg)
			if ce.status == http.StatusRequestEntityTooLarge {
				require.Greater(t, int64(longest), limit, "413 only for a line over the cap")
			}
			return
		}
		defer c.close()
		require.LessOrEqual(t, int64(longest), limit, "every accepted line is within the cap")
		require.NotEmpty(t, strings.TrimSpace(c.header.Summary))
		var regular int64
		for _, op := range c.ops {
			switch op.Kind {
			case repo.OpAdd:
				b, err := io.ReadAll(op.Content)
				require.NoError(t, err)
				regular += int64(len(b))
			case repo.OpAddLFS:
				require.Equal(t, strings.ToLower(op.OID), op.OID)
			case repo.OpDelete, repo.OpDeleteFolder:
			default:
				t.Fatalf("unexpected operation kind %q", op.Kind)
			}
		}
		require.Equal(t, regular, c.regularLen)
	})
}

// FuzzCommitRoundTrip builds a commit body the way huggingface_hub does
// (_prepare_commit_payload) and checks it parses back to the same
// operations.
func FuzzCommitRoundTrip(f *testing.F) {
	f.Add("Upload model", "Long\ndescription", "config.json", []byte(`{"a": 1}`), true, "weights/model.safetensors", strings.Repeat("ab", 32), int64(548105171), "logs/")
	f.Add("s", "", "notes.txt", []byte("plain text"), false, "a.bin", strings.Repeat("F", 64), int64(0), "")
	f.Add("ünïcode", "", "dir/ñ.txt", []byte{0xff, 0x00, 0x80}, false, "b.bin", "", int64(-1), "x/")
	f.Fuzz(func(t *testing.T, summary, description, path string, content []byte, useBase64 bool, lfsPath, oid string, size int64, folder string) {
		// JSON strings carry valid UTF-8 only; the client sends binary
		// content as base64.
		valid := func(s string) string { return strings.ToValidUTF8(s, "?") }
		summary, description, path, lfsPath, oid, folder = valid(summary), valid(description), valid(path), valid(lfsPath), valid(oid), valid(folder)
		if !utf8.Valid(content) {
			useBase64 = true
		}
		var body bytes.Buffer
		enc := json.NewEncoder(&body)
		write := func(key string, value any) {
			require.NoError(t, enc.Encode(map[string]any{"key": key, "value": value}))
		}
		write("header", map[string]any{"summary": summary, "description": description})
		file := map[string]any{"path": path, "content": string(content)}
		if useBase64 {
			file = map[string]any{"path": path, "content": base64.StdEncoding.EncodeToString(content), "encoding": "base64"}
		}
		write("file", file)
		write("lfsFile", map[string]any{"path": lfsPath, "algo": "sha256", "oid": oid, "size": size})
		write("deletedFile", map[string]any{"path": path})
		write("deletedFolder", map[string]any{"path": folder})

		c, err := fuzzHandler(int64(body.Len())).readCommit(&body)
		if strings.TrimSpace(summary) == "" {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		defer c.close()
		require.Equal(t, summary, c.header.Summary)
		require.Equal(t, description, c.header.Description)
		require.Len(t, c.ops, 4)
		require.Equal(t, repo.OpAdd, c.ops[0].Kind)
		require.Equal(t, path, c.ops[0].Path)
		got, err := io.ReadAll(c.ops[0].Content)
		require.NoError(t, err)
		require.True(t, bytes.Equal(content, got), "content round-trips")
		require.Equal(t, int64(len(content)), c.regularLen)
		require.Equal(t, repo.Operation{Kind: repo.OpAddLFS, Path: lfsPath, OID: strings.ToLower(oid), Size: size}, c.ops[1])
		require.Equal(t, repo.Operation{Kind: repo.OpDelete, Path: path}, c.ops[2])
		require.Equal(t, repo.Operation{Kind: repo.OpDeleteFolder, Path: folder}, c.ops[3])
	})
}

// FuzzReadLine checks the line reader against a plain split: lines come back
// whole and in order, and only a line over the cap fails.
func FuzzReadLine(f *testing.F) {
	f.Add([]byte("a\nbb\n\nccc"), uint8(4))
	f.Add([]byte(strings.Repeat("x", 40)+"\n"), uint8(16))
	f.Add([]byte(""), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, lineMax uint8) {
		limit := int64(lineMax)
		// The smallest buffer bufio allows, so long lines take several reads.
		br := bufio.NewReaderSize(bytes.NewReader(data), 16)
		want := lines(data)
		var got [][]byte
		for {
			line, err := readLine(br, limit)
			if err != nil && !errors.Is(err, io.EOF) {
				var ce *commitError
				require.True(t, errors.As(err, &ce))
				require.Equal(t, http.StatusRequestEntityTooLarge, ce.status)
				require.Less(t, len(got), len(want))
				require.Greater(t, int64(len(want[len(got)])), limit)
				return
			}
			if len(line) > 0 {
				require.LessOrEqual(t, int64(len(line)), limit)
				got = append(got, line)
			}
			if errors.Is(err, io.EOF) {
				break
			}
		}
		require.Equal(t, want, got)
	})
}

// cardSeeds are Model card shapes: with and without front matter, Windows
// line endings, a byte order mark, and broken YAML.
var cardSeeds = []string{
	"---\nlicense: mit\ntags: [a, b]\n---\n# Card\n",
	"\uFEFF---\r\nlicense: apache-2.0\r\n---\r\nbody",
	"---\n---\n",
	"---\n---",
	"---\na: 1\n---",
	"---\na: 1\n",
	"# No metadata\n",
	"---\nlicense: [mit\n---\n",
	"---\n- a\n- b\n---\n",
	"---\nscalar\n---\n",
	"---\na: &x [*x]\n---\n",
	"---\nmodel-index:\n- name: m\n  results: []\n---\n",
	"",
}

// FuzzFrontMatter checks FrontMatter's result is the text between the
// opening and the first closing `---` line, and that parsing it never
// panics.
func FuzzFrontMatter(f *testing.F) {
	for _, s := range cardSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, card string) {
		front, found := FrontMatter(card)
		s := strings.ReplaceAll(strings.TrimPrefix(card, "\uFEFF"), "\r\n", "\n")
		if !found {
			require.Empty(t, front)
			if strings.HasPrefix(s, "---\n") {
				body := s[len("---\n"):]
				require.NotContains(t, body, "\n---\n")
				require.False(t, strings.HasSuffix(body, "\n---"))
			}
			return
		}
		require.True(t, strings.HasPrefix(s, "---\n"+front))
		rest := s[len("---\n"+front):]
		if front == "" {
			require.True(t, strings.HasPrefix(rest, "---\n") || rest == "---" || strings.HasPrefix(rest, "\n---\n") || rest == "\n---", "rest %q", rest)
		} else {
			require.True(t, strings.HasPrefix(rest, "\n---\n") || rest == "\n---", "rest %q", rest)
			require.NotContains(t, front, "\n---\n")
		}
		data, err := ParseCardData(front)
		if err == nil {
			require.NotNil(t, data)
		}
	})
}

// FuzzFrontMatterRoundTrip wraps arbitrary YAML text in `---` lines and
// checks FrontMatter gives the same text back.
func FuzzFrontMatterRoundTrip(f *testing.F) {
	f.Add("license: mit", "# Card\n", true, false)
	f.Add("a: 1\r\nb: 2", "", false, true)
	f.Add("x: '---'", "---\nmore", false, false)
	f.Fuzz(func(t *testing.T, front, rest string, bom, crlf bool) {
		front = strings.ReplaceAll(front, "\r", "")
		if front == "" || front == "---" || strings.HasPrefix(front, "---\n") ||
			strings.Contains(front, "\n---\n") || strings.HasSuffix(front, "\n---") {
			// These are not one front matter block: the first `---` line
			// would end it early.
			return
		}
		card := "---\n" + front + "\n---\n" + rest
		if crlf {
			card = strings.ReplaceAll(card, "\n", "\r\n")
		}
		if bom {
			card = "\uFEFF" + card
		}
		got, found := FrontMatter(card)
		require.True(t, found)
		require.Equal(t, front, got)
	})
}

// FuzzValidateYAML checks POST /api/validate-yaml always answers JSON with
// 200 or 400, whatever the card (contract §7.4).
func FuzzValidateYAML(f *testing.F) {
	for _, s := range cardSeeds {
		f.Add(s)
	}
	h := fuzzHandler(1 << 20)
	f.Fuzz(func(t *testing.T, card string) {
		body, err := json.Marshal(map[string]string{"content": card, "repoType": "model"})
		require.NoError(t, err)
		r := hubRequest(http.MethodPost)
		r.Body = io.NopCloser(bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.validateYAML(w, r)
		require.Contains(t, []int{http.StatusOK, http.StatusBadRequest}, w.Code)
		require.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
		var out struct {
			Errors   []map[string]string `json:"errors"`
			Warnings []map[string]string `json:"warnings"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		require.NotNil(t, out.Errors)
		require.NotNil(t, out.Warnings)
		require.Equal(t, w.Code == http.StatusBadRequest, len(out.Errors) > 0)
	})
}

// TestReadCommitRemovesSpooledFiles checks the commit parser leaves nothing
// behind in the temporary directory when a body fails part way.
func TestReadCommitRemovesSpooledFiles(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	h := fuzzHandler(spoolMemory + 1<<20)
	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), spoolMemory/2+1))
	body := `{"key":"header","value":{"summary":"s"}}` + "\n" +
		`{"key":"file","value":{"path":"a","content":"` + big + `","encoding":"base64"}}` + "\n" +
		`{"key":"file","value":{"path":"b","content":"` + big + `","encoding":"base64"}}` + "\n" +
		`{"key":"file","value":{"path":"c","content":"` + big + `","encoding":"base64"}}` + "\n" +
		`{"key":"nope","value":{}}`
	_, err := h.readCommit(strings.NewReader(body))
	require.Error(t, err)
	left, err := os.ReadDir(os.Getenv("TMPDIR"))
	require.NoError(t, err)
	require.Empty(t, left, "spooled content is removed when the body is refused")
}
