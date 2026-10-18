package convert

import (
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/hangar-hub/hangar/internal/datasets/features"
	"github.com/hangar-hub/hangar/internal/datasets/layout"
)

// mediaBatch is the writer batch size the Hub uses for image and audio
// datasets.
const mediaBatch = 100

// maxMediaFile caps one image or audio file.
const maxMediaFile = 1 << 31

var metadataNames = []string{"metadata.csv", "metadata.jsonl", "metadata.parquet"}

// folderBuilder is ImageFolder or AudioFolder.
type folderBuilder struct {
	module string
	column string
	kind   features.Kind
}

func (b folderBuilder) name() string { return b.module }

// isData reports whether a file is an image or audio file, also inside a
// compressed file such as x.wav.gz. Inside a zip archive the builder takes
// only the lower-case extensions it lists, and no compressed files.
func (b folderBuilder) isData(f File) bool {
	if f.archive != "" {
		ext := path.Ext(f.entry)
		return ext == strings.ToLower(ext) && layout.ModuleForExtension(ext) == b.module
	}
	p := f.Path
	ext := strings.ToLower(path.Ext(p))
	second := strings.ToLower(path.Ext(strings.TrimSuffix(p, path.Ext(p))))
	return layout.ModuleForExtension(ext) == b.module || (second != "" && layout.ModuleForExtension(second) == b.module)
}

// label is the folder name FolderBasedBuilder takes as a file's class
// label when it analyses the files. `datasets` names a file in an archive
// zip://{entry}::{archive}, so the label is the entry's folder, and empty
// at the top of the archive.
func label(f File) string {
	if f.archive != "" {
		if !strings.Contains(f.entry, "/") {
			return ""
		}
		return path.Base(path.Dir(f.entry))
	}
	return path.Base(path.Dir(f.Path))
}

// depth is count_path_segments: the slashes in the name `datasets` gives
// a file, counted from the Repo root, which adds the same number to every
// file. For a file in an archive that name is zip://{entry}::{archive}.
func depth(f File) int {
	if f.archive != "" {
		return 2 + strings.Count(f.entry, "/") + strings.Count(f.archive, "/")
	}
	return strings.Count(f.Path, "/")
}

// mediaName is the file name the builder stores with an image or audio
// value: for a file in an archive, the base name of zip://{entry}::{archive}
// is the archive's.
func mediaName(f File) string {
	if f.archive != "" {
		return path.Base(f.archive)
	}
	return path.Base(innerName(f.Path))
}

// folderPlan is FolderBasedBuilder's analysis of every Split.
type folderPlan struct {
	metadata  map[string][]File
	addLabels bool
	labels    []string
	metaExt   string
	// files are each Split's files with every zip archive replaced by the
	// files in it: the Split's own files first, then each archive's, in
	// the order the builder reads them.
	files map[string][]File
	// entries are the files in each archive by path, for metadata files
	// in an archive, whose file names resolve inside it.
	entries map[string]map[string]File
}

// expand lists a Split's files as the builder reads them: images, audio
// and metadata files first, in order, then the files of each zip archive.
func (p *folderPlan) expand(e *env, files []File) ([]File, error) {
	var out, archives []File
	for _, f := range files {
		if strings.EqualFold(path.Ext(f.Path), ".zip") {
			archives = append(archives, f)
			continue
		}
		out = append(out, f)
	}
	for _, a := range archives {
		in, err := archiveFiles(e.ctx, a)
		if err != nil {
			return nil, err
		}
		byPath := map[string]File{}
		for _, f := range in {
			byPath[f.Path] = f
		}
		p.entries[a.Path] = byPath
		out = append(out, in...)
	}
	return out, nil
}

func (b folderBuilder) plan(e *env, splits []splitFiles) (*folderPlan, error) {
	p := &folderPlan{metadata: map[string][]File{}, files: map[string][]File{}, entries: map[string]map[string]File{}}
	labels := map[string]bool{}
	depths := map[int]bool{}
	exts := map[string]bool{}
	for _, s := range splits {
		files, err := p.expand(e, s.files)
		if err != nil {
			return nil, err
		}
		p.files[s.name] = files
		for _, f := range files {
			switch {
			case b.isData(f):
				labels[label(f)] = true
				depths[depth(f)] = true
			case slices.Contains(metadataNames, path.Base(f.Path)):
				p.metadata[s.name] = append(p.metadata[s.name], f)
				exts[path.Ext(f.Path)] = true
			}
		}
	}
	if len(p.metadata) > 0 {
		if len(exts) > 1 {
			return nil, fmt.Errorf("found metadata files with different extensions: %v", sortedKeys(exts))
		}
		for e := range exts {
			p.metaExt = e
		}
		return p, nil
	}
	if len(labels) > 1 && len(depths) == 1 {
		p.addLabels = true
		p.labels = sortedKeys(labels)
	}
	return p, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (b folderBuilder) base() *features.Feature { return &features.Feature{Kind: b.kind, Length: -1} }

// metadataTables streams a metadata file's rows as decoded objects, in the
// chunks the builder reads them.
func metadataTables(e *env, f File, ext string, fn func(cols []string, rows [][]any, types []*itype) error) error {
	switch ext {
	case ".csv":
		o, err := parseCSVOptions(nil)
		if err != nil {
			return err
		}
		ms, err := members(e.ctx, f)
		if err != nil {
			return err
		}
		for _, m := range ms {
			rc, err := m.open()
			if err != nil {
				return err
			}
			cr, err := newCSVReader(rc, o)
			if err != nil {
				_ = rc.Close()
				return err
			}
			for {
				chunk, err := cr.next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					_ = rc.Close()
					return err
				}
				ctypes := chunkTypes(chunk, cr.names, o)
				types := make([]*itype, len(ctypes))
				for i, t := range ctypes {
					types[i] = &itype{id: t.ID}
				}
				rows := make([][]any, len(chunk))
				for r, rec := range chunk {
					row := make([]any, len(cr.names))
					for c := range cr.names {
						cell, present := "", c < len(rec)
						if present {
							cell = rec[c]
						}
						v, err := csvValue(cell, present, ctypes[c], o)
						if err != nil {
							_ = rc.Close()
							return err
						}
						row[c] = v
					}
					rows[r] = row
				}
				if err := fn(cr.names, rows, types); err != nil {
					_ = rc.Close()
					return err
				}
			}
			_ = rc.Close()
		}
		return nil
	case ".jsonl":
		ms, err := members(e.ctx, f)
		if err != nil {
			return err
		}
		for _, m := range ms {
			err := jsonChunks(m, "", func(ex []any) error {
				t := &itype{id: features.Struct}
				for _, x := range ex {
					t = merge(t, inferJSON(x, true, 0))
				}
				if t.json || t.id != features.Struct {
					return errors.New("the metadata file has values of different types in a column")
				}
				for _, fl := range t.fields {
					if fl.typ.json {
						return fmt.Errorf("the metadata column %q has values of different types", fl.name)
					}
				}
				cols := make([]string, len(t.fields))
				types := make([]*itype, len(t.fields))
				for i, fl := range t.fields {
					cols[i], types[i] = fl.name, fl.typ
				}
				rows := make([][]any, len(ex))
				for r, x := range ex {
					obj, ok := x.(*orderedObject)
					if !ok {
						return errors.New("each metadata line must be a JSON object")
					}
					row := make([]any, len(cols))
					for i, c := range cols {
						row[i], _ = obj.get(c)
					}
					rows[r] = row
				}
				return fn(cols, rows, types)
			})
			if err != nil {
				return err
			}
		}
		return nil
	case ".parquet":
		pf, err := openParquet(e.ctx, f)
		if err != nil {
			return err
		}
		cols := pf.Features.Names()
		types := make([]*itype, len(cols))
		for i, fl := range pf.Schema.Fields {
			types[i] = &itype{id: fl.Type.ID}
		}
		idx, _ := pf.Columns(nil)
		var start int64
		for g := range pf.NumRowGroups() {
			n := pf.RowGroupRows(g)
			rows, err := pf.ReadRows(idx, start, n)
			if err != nil {
				return err
			}
			start += n
			if err := fn(cols, rows, types); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%w: metadata file %s", ErrUnsupported, f.Path)
}

// metadataFeatures maps file_name columns to the base feature, as
// _set_feature does.
func (b folderBuilder) metadataFeatures(fs features.Features) (features.Features, error) {
	found := false
	out := make(features.Features, 0, len(fs))
	for _, f := range fs {
		key := f.Name
		isStr := f.Feature.Kind == features.KindValue && (f.Feature.Dtype == "string" || f.Feature.Dtype == "large_string")
		isStrList := f.Feature.IsListKind() && f.Feature.Elem.Kind == features.KindValue &&
			(f.Feature.Elem.Dtype == "string" || f.Feature.Elem.Dtype == "large_string")
		switch {
		case (key == "file_name" || strings.HasSuffix(key, "_file_name")) && isStr:
			name := strings.TrimSuffix(strings.TrimSuffix(key, "file_name"), "_")
			if key == "file_name" {
				name = b.column
			}
			out = append(out, features.Field{Name: name, Feature: b.base()})
			found = true
		case (key == "file_names" || strings.HasSuffix(key, "_file_names")) && isStrList:
			name := strings.TrimSuffix(strings.TrimSuffix(key, "file_names"), "_")
			if key == "file_names" {
				name = b.column + "s"
			}
			out = append(out, features.Field{Name: name, Feature: &features.Feature{Kind: f.Feature.Kind, Elem: b.base(), Length: -1}})
			found = true
		default:
			out = append(out, f)
		}
	}
	if !found {
		return nil, errors.New("`file_name`, `*_file_name`, `file_names` or `*_file_names` must be present as dictionary key in metadata files")
	}
	return out, nil
}

func (b folderBuilder) infer(e *env, splits []splitFiles) (features.Features, error) {
	p, err := b.plan(e, splits)
	if err != nil {
		return nil, err
	}
	if len(p.metadata) > 0 {
		for _, s := range splits {
			files := p.metadata[s.name]
			if len(files) == 0 {
				continue
			}
			var fs features.Features
			stop := errors.New("stop")
			err := metadataTables(e, files[0], p.metaExt, func(cols []string, _ [][]any, types []*itype) error {
				for i, c := range cols {
					fs = append(fs, features.Field{Name: c, Feature: types[i].toFeature()})
				}
				return stop
			})
			if err != nil && !errors.Is(err, stop) {
				return nil, fmt.Errorf("%s: %w", files[0].Path, err)
			}
			if fs != nil {
				return b.metadataFeatures(fs)
			}
		}
		return nil, errors.New("the metadata files are empty")
	}
	if p.addLabels {
		return features.Features{{Name: b.column, Feature: b.base()},
			{Name: "label", Feature: &features.Feature{Kind: features.KindClassLabel, Names: p.labels, Length: -1}}}, nil
	}
	return features.Features{{Name: b.column, Feature: b.base()}}, nil
}

// resolveFileName joins a metadata file_name to the metadata file's folder.
func resolveFileName(metaPath, name string) (string, error) {
	if strings.Contains(name, "://") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("invalid metadata file_name %q: it must be a relative path inside the folder of the metadata file", name)
	}
	rel := path.Clean(strings.ReplaceAll(name, `\`, "/"))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("invalid metadata file_name %q: it must not leave the folder of the metadata file", name)
	}
	dir := path.Dir(metaPath)
	if dir == "." {
		return rel, nil
	}
	return path.Join(dir, rel), nil
}

// readFile reads the file a metadata file names: a file of the Repo, or,
// for a metadata file in a zip archive, a file of the same archive.
func (b folderBuilder) readFile(e *env, plan *folderPlan, meta File, p string) ([]any, error) {
	var f File
	if meta.archive != "" {
		var ok bool
		if f, ok = plan.entries[meta.archive][p]; !ok {
			return nil, fmt.Errorf("%s is not in %s", strings.TrimPrefix(p, meta.archive+"/"), meta.archive)
		}
	} else {
		var err error
		if f, err = e.open(e.ctx, p); err != nil {
			return nil, err
		}
	}
	return b.readData(e, f)
}

// readData reads an image or audio file as the builder stores it: its
// bytes and its file name.
func (b folderBuilder) readData(e *env, f File) ([]any, error) {
	p := f.Path
	rc, err := f.Open(e.ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var r io.Reader = rc
	if f.archive == "" {
		dr, _, err := decompress(p, rc)
		if err != nil {
			return nil, err
		}
		r = dr
	}
	raw, err := readAllLimit(r, maxMediaFile)
	if err != nil {
		return nil, err
	}
	return []any{raw, mediaName(f)}, nil
}

func (b folderBuilder) read(e *env, split splitFiles, fs features.Features, emit emitFunc) error {
	all := e.splits
	if len(all) == 0 {
		all = []splitFiles{split}
	}
	p, err := b.plan(e, all)
	if err != nil {
		return err
	}
	st, err := fs.StorageSchema()
	if err != nil {
		return err
	}
	var batch [][]any
	add := func(row []any) error {
		batch = append(batch, row)
		if len(batch) >= mediaBatch {
			err := emit(batch)
			batch = nil
			return err
		}
		return nil
	}
	if len(p.metadata) > 0 {
		for _, mf := range p.metadata[split.name] {
			err := metadataTables(e, mf, p.metaExt, func(cols []string, rows [][]any, _ []*itype) error {
				for _, r := range rows {
					row := make([]any, len(fs))
					for i, fl := range fs {
						var v any
						switch {
						case fl.Feature.Kind == b.kind:
							v = lookupFileColumn(cols, r, fl.Name, b.column, false)
							if v != nil {
								name, ok := v.(string)
								if !ok {
									return fmt.Errorf("the %s file_name is not a string", fl.Name)
								}
								rp, err := resolveFileName(mf.Path, name)
								if err != nil {
									return err
								}
								if v, err = b.readFile(e, p, mf, rp); err != nil {
									return fmt.Errorf("%s: %w", rp, err)
								}
							}
						case fl.Feature.IsListKind() && fl.Feature.Elem.Kind == b.kind:
							lv := lookupFileColumn(cols, r, fl.Name, b.column, true)
							if lv != nil {
								names, ok := lv.([]any)
								if !ok {
									return fmt.Errorf("the %s file_names are not a list", fl.Name)
								}
								list := make([]any, len(names))
								for k, n := range names {
									s, ok := n.(string)
									if !ok {
										continue
									}
									rp, err := resolveFileName(mf.Path, s)
									if err != nil {
										return err
									}
									if list[k], err = b.readFile(e, p, mf, rp); err != nil {
										return fmt.Errorf("%s: %w", rp, err)
									}
								}
								v = list
							}
						default:
							for c, name := range cols {
								if name == fl.Name {
									v = r[c]
								}
							}
						}
						c, err := CastFeature(v, fl.Feature, st.Fields[i].Type)
						if err != nil {
							return fmt.Errorf("column %q: %w", fl.Name, err)
						}
						row[i] = c
					}
					if err := add(row); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("%s: %w", mf.Path, err)
			}
		}
	} else {
		labelIdx := fs.Index("label")
		for _, f := range p.files[split.name] {
			// The builder reads every file of an archive as data, whatever
			// its extension; only the Split's own files are filtered.
			if f.archive == "" && !b.isData(f) {
				continue
			}
			v, err := b.readData(e, f)
			if err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
			row := make([]any, len(fs))
			row[fs.Index(b.column)] = v
			if labelIdx >= 0 && p.addLabels {
				// The builder labels a file of an archive with the archive's
				// folder, which is not one of the labels it found inside.
				name := label(f)
				if f.archive != "" {
					name = path.Base(path.Dir(f.archive))
				}
				i := slices.Index(p.labels, name)
				if i < 0 {
					return fmt.Errorf("invalid string class label %s", name)
				}
				row[labelIdx] = int64(i)
			}
			if err := add(row); err != nil {
				return err
			}
		}
	}
	if len(batch) > 0 {
		return emit(batch)
	}
	return nil
}

// lookupFileColumn finds the file_name (or file_names) value of a column.
func lookupFileColumn(cols []string, row []any, feature, base string, list bool) any {
	suffix := "_file_name"
	plain := "file_name"
	if list {
		suffix, plain = "_file_names", "file_names"
	}
	want := feature + suffix
	for c, name := range cols {
		if name == want {
			return row[c]
		}
	}
	if feature == base || (list && feature == base+"s") {
		for c, name := range cols {
			if name == plain {
				return row[c]
			}
		}
	}
	return nil
}
