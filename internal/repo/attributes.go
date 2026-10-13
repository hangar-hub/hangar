package repo

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// LFSRules decides which paths a .gitattributes file stores in LFS: the
// paths whose `filter` attribute is `lfs`. It follows gitattributes(5): a
// pattern without a slash matches a file name at any depth, a pattern with a
// slash matches the path from the Repo root, `**` matches across
// directories, and the last line that mentions `filter` wins.
type LFSRules struct {
	rules []attrRule
}

type attrRule struct {
	re *regexp.Regexp
	// lfs is true for filter=lfs, false for any other value, -filter or
	// !filter.
	lfs bool
}

// ParseLFSRules parses a .gitattributes file. Lines it cannot parse are
// ignored, as git ignores them.
func ParseLFSRules(data []byte) *LFSRules {
	r := &LFSRules{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pattern, rest := splitPattern(line)
		if pattern == "" || strings.HasPrefix(pattern, "!") || strings.HasPrefix(pattern, "[attr]") {
			continue
		}
		val, ok := filterValue(rest)
		if !ok {
			continue
		}
		re, err := compileAttrPattern(pattern)
		if err != nil {
			continue
		}
		r.rules = append(r.rules, attrRule{re: re, lfs: val == "lfs"})
	}
	return r
}

// IsLFS reports whether p (a clean path from the Repo root) is stored in LFS.
func (r *LFSRules) IsLFS(p string) bool {
	if r == nil {
		return false
	}
	for i := len(r.rules) - 1; i >= 0; i-- {
		if r.rules[i].re.MatchString(p) {
			return r.rules[i].lfs
		}
	}
	return false
}

// splitPattern separates the pattern from the attributes, handling a
// C-style quoted pattern.
func splitPattern(line string) (string, string) {
	if strings.HasPrefix(line, `"`) {
		for i := 1; i < len(line); i++ {
			if line[i] == '\\' {
				i++
				continue
			}
			if line[i] == '"' {
				p, err := strconv.Unquote(line[:i+1])
				if err != nil {
					return "", ""
				}
				return p, line[i+1:]
			}
		}
		return "", ""
	}
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], line[i+1:]
}

// filterValue returns the `filter` attribute a line sets, and whether the
// line mentions it at all.
func filterValue(attrs string) (string, bool) {
	val, found := "", false
	for _, a := range strings.Fields(attrs) {
		switch {
		case a == "filter":
			val, found = "true", true
		case a == "-filter" || a == "!filter":
			val, found = "", true
		case strings.HasPrefix(a, "filter="):
			val, found = strings.TrimPrefix(a, "filter="), true
		}
	}
	return val, found
}

// compileAttrPattern turns a gitattributes pattern into an anchored regular
// expression over the full path.
func compileAttrPattern(p string) (*regexp.Regexp, error) {
	p = strings.TrimSuffix(p, "/")
	anchored := strings.Contains(p, "/")
	p = strings.TrimPrefix(p, "/")
	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		// A pattern without a slash matches the name at any depth.
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				atStart := i == 0 || p[i-1] == '/'
				atEnd := i+2 == len(p)
				switch {
				case atStart && i+2 < len(p) && p[i+2] == '/':
					// "**/" matches zero or more directories.
					b.WriteString("(?:.*/)?")
					i += 2
				case atStart && atEnd && i > 0:
					// "/**" matches everything inside.
					b.WriteString(".*")
					i++
				default:
					b.WriteString("[^/]*")
					i++
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := i + 1
			if j < len(p) && (p[j] == '!' || p[j] == '^') {
				j++
			}
			if j < len(p) && p[j] == ']' {
				j++
			}
			for j < len(p) && p[j] != ']' {
				j++
			}
			if j >= len(p) {
				b.WriteString(regexp.QuoteMeta(string(c)))
				continue
			}
			class := p[i+1 : j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = j
		case '\\':
			if i+1 < len(p) {
				i++
				b.WriteString(regexp.QuoteMeta(string(p[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
