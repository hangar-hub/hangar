package convert

import (
	"context"
	"fmt"

	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
)

// emitFunc receives a batch of rows, one storage value per feature. A
// batch is one table as `datasets` writes it, which matters for the
// num_bytes it reports.
type emitFunc func(rows [][]any) error

// splitFiles is a Split and its data files.
type splitFiles struct {
	name  string
	files []File
}

// env is what builders need besides the files.
type env struct {
	ctx  context.Context
	opts map[string]any
	// open returns any file of the Repo at the Commit by path, for
	// folder builders whose metadata names files.
	open func(ctx context.Context, path string) (File, error)
	// repoURL is hf://datasets/{ns}/{name}@{commit}, as `datasets` names
	// the files it reads.
	repoURL string
	// splits are every Split of the config, which folder builders analyse
	// together.
	splits []splitFiles
	// budget limits the decoded Parquet data a read holds, or nil.
	budget *pq.Budget
	// nullStructChildren is set by a builder while it emits rows that
	// `datasets` reads with pyarrow's JSON reader, where the children of
	// a null struct are null, which changes num_bytes.
	nullStructChildren bool
}

// builder is a `datasets` packaged builder.
type builder interface {
	name() string
	// infer returns the Features `datasets` infers when the card declares none.
	infer(e *env, splits []splitFiles) (features.Features, error)
	// read streams a Split's rows cast to the Features.
	read(e *env, split splitFiles, fs features.Features, emit emitFunc) error
}

// builderFor returns the builder of a name.
func builderFor(name string) (builder, error) {
	switch name {
	case "csv":
		return csvBuilder{}, nil
	case "json":
		return jsonBuilder{}, nil
	case "text":
		return textBuilder{}, nil
	case "parquet":
		return parquetBuilder{}, nil
	case "arrow":
		return arrowBuilder{}, nil
	case "webdataset":
		return webdatasetBuilder{}, nil
	case "imagefolder":
		return folderBuilder{module: "imagefolder", column: "image", kind: features.KindImage}, nil
	case "audiofolder":
		return folderBuilder{module: "audiofolder", column: "audio", kind: features.KindAudio}, nil
	}
	return nil, fmt.Errorf("%w: Hangar cannot convert the %q format to Parquet", ErrUnsupported, name)
}
