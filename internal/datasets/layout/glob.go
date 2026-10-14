package layout

import (
	"errors"
	"regexp"
	"strings"
)

// ErrInvalidPattern means a data_files glob cannot be translated.
var ErrInvalidPattern = errors.New("invalid data_files pattern")

// globRegexp translates a glob to a regular expression the way fsspec's
// glob_translate does (fsspec/utils.py, copied from CPython's
// glob.translate with recursive=True, include_hidden=True and "/" as the
// only separator), which is what `datasets` uses to match data files
// against a Repo's tree.
func globRegexp(pat string) (*regexp.Regexp, error) {
	const (
		anySep          = "/"
		notSep          = "[^/]"
		oneLastSegment  = notSep + "+"
		oneSegment      = oneLastSegment + anySep
		anySegments     = "(?:.+" + anySep + ")?"
		anyLastSegments = ".*"
	)
	var b strings.Builder
	parts := strings.Split(pat, "/")
	last := len(parts) - 1
	for idx, part := range parts {
		switch {
		case part == "*":
			if idx < last {
				b.WriteString(oneSegment)
			} else {
				b.WriteString(oneLastSegment)
			}
			continue
		case part == "**":
			if idx < last {
				b.WriteString(anySegments)
			} else {
				b.WriteString(anyLastSegments)
			}
			continue
		case strings.Contains(part, "**"):
			return nil, errors.Join(ErrInvalidPattern, errors.New("'**' can only be an entire path component"))
		}
		if part != "" {
			b.WriteString(translatePart(part, notSep+"*", notSep))
		}
		if idx < last {
			b.WriteString(anySep)
		}
	}
	re, err := regexp.Compile(`^(?s:` + b.String() + `)\z`)
	if err != nil {
		return nil, errors.Join(ErrInvalidPattern, err)
	}
	return re, nil
}

// translatePart is CPython's fnmatch._translate for one path component.
func translatePart(pattern, star, question string) string {
	pat := []rune(pattern)
	var res []string
	n := len(pat)
	i := 0
	for i < n {
		c := pat[i]
		i++
		switch c {
		case '*':
			if len(res) == 0 || res[len(res)-1] != star {
				res = append(res, star)
			}
		case '?':
			res = append(res, question)
		case '[':
			j := i
			if j < n && pat[j] == '!' {
				j++
			}
			if j < n && pat[j] == ']' {
				j++
			}
			for j < n && pat[j] != ']' {
				j++
			}
			if j >= n {
				res = append(res, `\[`)
				continue
			}
			stuff := string(pat[i:j])
			if !strings.Contains(stuff, "-") {
				stuff = strings.ReplaceAll(stuff, `\`, `\\`)
			} else {
				var chunks []string
				k := i + 1
				if pat[i] == '!' {
					k = i + 2
				}
				for {
					rel := runeIndex(pat[min(k, j):j], '-')
					if rel < 0 {
						break
					}
					k = min(k, j) + rel
					chunks = append(chunks, string(pat[i:k]))
					i = k + 1
					k += 3
				}
				chunk := string(pat[i:j])
				if chunk != "" {
					chunks = append(chunks, chunk)
				} else {
					chunks[len(chunks)-1] += "-"
				}
				// Remove empty ranges, which are invalid in a regexp.
				for k := len(chunks) - 1; k > 0; k-- {
					prev, cur := []rune(chunks[k-1]), []rune(chunks[k])
					if len(prev) > 0 && len(cur) > 0 && prev[len(prev)-1] > cur[0] {
						chunks[k-1] = string(prev[:len(prev)-1]) + string(cur[1:])
						chunks = append(chunks[:k], chunks[k+1:]...)
					}
				}
				for k := range chunks {
					chunks[k] = strings.ReplaceAll(strings.ReplaceAll(chunks[k], `\`, `\\`), "-", `\-`)
				}
				stuff = strings.Join(chunks, "-")
			}
			// Escape set operations, which Python reserves.
			stuff = regexp.MustCompile(`([&~|])`).ReplaceAllString(stuff, `\$1`)
			i = j + 1
			switch stuff {
			case "":
				// An empty range never matches. RE2 has no (?!), so use a
				// class that matches nothing.
				res = append(res, `[^\x00-\x{10FFFF}]`)
			case "!":
				res = append(res, ".")
			default:
				switch stuff[0] {
				case '!':
					stuff = "^" + stuff[1:]
				case '^', '[':
					stuff = `\` + stuff
				}
				res = append(res, "["+stuff+"]")
			}
		default:
			res = append(res, regexp.QuoteMeta(string(c)))
		}
	}
	return strings.Join(res, "")
}

func runeIndex(rs []rune, r rune) int {
	for i, x := range rs {
		if x == r {
			return i
		}
	}
	return -1
}

// hasMagic reports whether a pattern has glob characters, as glob.has_magic.
func hasMagic(p string) bool { return strings.ContainsAny(p, "*?[") }
