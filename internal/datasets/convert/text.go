package convert

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jamesagarside/hangar/internal/datasets/features"
)

type textBuilder struct{}

func (textBuilder) name() string { return "text" }

func (textBuilder) infer(e *env, _ []splitFiles) (features.Features, error) {
	if s, ok := optString(e.opts, "sample_by"); ok && s != "line" && s != "paragraph" && s != "document" {
		return nil, fmt.Errorf("%w: sample_by %q", ErrUnsupported, s)
	}
	return features.Features{{Name: "text", Feature: features.Value("string")}}, nil
}

// universalNewlines converts \r\n and \r to \n, as Python's text mode does.
func universalNewlines(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
}

func (b textBuilder) read(e *env, split splitFiles, fs features.Features, emit emitFunc) error {
	if len(fs) != 1 {
		return fmt.Errorf("the text builder needs exactly one feature, got %v", fs.Names())
	}
	st, err := fs[0].Feature.Storage()
	if err != nil {
		return err
	}
	sampleBy, _ := optString(e.opts, "sample_by")
	keep, _ := e.opts["keep_linebreaks"].(bool)
	for _, f := range split.files {
		ms, err := members(e.ctx, f)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if err := b.readMember(m, sampleBy, keep, st, emit); err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
		}
	}
	return nil
}

func (b textBuilder) readMember(m member, sampleBy string, keep bool, st *features.Arrow, emit emitFunc) error {
	rc, err := m.open()
	if err != nil {
		return err
	}
	defer rc.Close()
	toRows := func(samples []string) error {
		rows := make([][]any, len(samples))
		for i, s := range samples {
			v, err := Cast(s, st)
			if err != nil {
				return err
			}
			rows[i] = []any{v}
		}
		if len(rows) == 0 {
			return nil
		}
		return emit(rows)
	}
	if sampleBy == "document" {
		raw, err := readAllLimit(rc, maxJSONDocument)
		if err != nil {
			return err
		}
		return toRows([]string{string(universalNewlines(raw))})
	}
	cr := newChunkReader(rc, jsonChunkBytes)
	carry := ""
	for {
		chunk, err := cr.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		text := string(universalNewlines(chunk))
		if sampleBy == "paragraph" {
			parts := strings.Split(carry+text, "\n\n")
			carry = parts[len(parts)-1]
			var samples []string
			for _, p := range parts[:len(parts)-1] {
				if p != "" {
					samples = append(samples, p)
				}
			}
			if err := toRows(samples); err != nil {
				return err
			}
			continue
		}
		lines := strings.SplitAfter(text, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if !keep {
			for i, l := range lines {
				lines[i] = strings.TrimRight(l, "\n")
			}
		}
		if err := toRows(lines); err != nil {
			return err
		}
	}
	if sampleBy == "paragraph" && carry != "" {
		return toRows([]string{carry})
	}
	return nil
}
