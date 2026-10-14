package convert

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// maxCSVField caps one CSV field, so a stray quote cannot buffer a whole file.
const maxCSVField = 256 << 20

// csvTokenizer splits CSV records the way pandas' C tokenizer does with
// its defaults (doublequote, no escape character, minimal quoting): a
// quote only starts a quoted field at the start of a field, characters
// after a closing quote are appended to the field, quotes inside an
// unquoted field are literal, \n, \r\n and \r end records, and blank
// lines are skipped.
type csvTokenizer struct {
	r       *bufio.Reader
	sep     rune
	quote   rune
	comment rune
	line    int
}

func newCSVTokenizer(r io.Reader, sep, quote, comment rune) *csvTokenizer {
	return &csvTokenizer{r: bufio.NewReaderSize(r, 1<<16), sep: sep, quote: quote, comment: comment}
}

const (
	stStartRecord = iota
	stStartField
	stInField
	stInQuoted
	stQuoteInQuoted
	stComment
)

// errEOFInQuote is pandas' "EOF inside string" error.
var errEOFInQuote = errors.New("error tokenizing data: EOF inside string")

// read returns the next record, or io.EOF.
func (t *csvTokenizer) read() ([]string, error) {
	var fields []string
	var field strings.Builder
	state := stStartRecord
	endField := func() {
		fields = append(fields, field.String())
		field.Reset()
	}
	for {
		c, _, err := t.r.ReadRune()
		if errors.Is(err, io.EOF) {
			switch state {
			case stStartRecord, stComment:
				if fields == nil {
					return nil, io.EOF
				}
				return fields, nil
			case stInQuoted:
				return nil, errEOFInQuote
			}
			endField()
			return fields, nil
		}
		if err != nil {
			return nil, err
		}
		if field.Len() > maxCSVField {
			return nil, errors.New("error tokenizing data: a field is larger than 256 MiB")
		}
		if state == stInQuoted && c != t.quote {
			// Quoted values keep their characters, line breaks included.
			if c == '\n' {
				t.line++
			}
			field.WriteRune(c)
			continue
		}
		eol := c == '\n' || c == '\r'
		if eol && c == '\r' {
			if next, _, err := t.r.ReadRune(); err == nil && next != '\n' {
				_ = t.r.UnreadRune()
			}
		}
		switch state {
		case stStartRecord:
			switch {
			case eol:
				t.line++
				continue // a blank line
			case t.comment != 0 && c == t.comment:
				state = stComment
				continue
			}
			state = stStartField
			fallthrough
		case stStartField:
			switch {
			case c == t.quote:
				state = stInQuoted
			case c == t.sep:
				endField()
			case eol:
				t.line++
				endField()
				return fields, nil
			case t.comment != 0 && c == t.comment:
				endField()
				state = stComment
			default:
				field.WriteRune(c)
				state = stInField
			}
		case stInField:
			switch {
			case c == t.sep:
				endField()
				state = stStartField
			case eol:
				t.line++
				endField()
				return fields, nil
			case t.comment != 0 && c == t.comment:
				endField()
				state = stComment
			default:
				field.WriteRune(c)
			}
		case stInQuoted:
			state = stQuoteInQuoted
		case stQuoteInQuoted:
			switch {
			case c == t.quote:
				field.WriteRune(c)
				state = stInQuoted
			case c == t.sep:
				endField()
				state = stStartField
			case eol:
				t.line++
				endField()
				return fields, nil
			default:
				field.WriteRune(c)
				state = stInField
			}
		case stComment:
			if eol {
				t.line++
				if fields == nil {
					state = stStartRecord
					continue
				}
				return fields, nil
			}
		}
	}
}
