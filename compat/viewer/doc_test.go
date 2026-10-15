//go:build compat

// Package viewer is the compat/viewer suite (docs/spec/14-datasets.md
// "Tests"): it converts small public datasets in Hangar and compares the
// Dataset Viewer's responses with the Hub's for the same Revision, checks
// Config and Split resolution against the `datasets` library, and
// validates Croissant output with mlcroissant. Run it with
// `go test -tags compat ./compat/viewer`.
//
// Each test runs on every backend in backends: the pipeline and the Viewer
// over fakes, the hangar binary on SQLite, and the hangar binary on
// Postgres when dbtest's Postgres tests are on. On the binary, datasets go
// up with `hangar push`, the Instance's own Job worker converts them, and
// the responses come through cmd/hangar's routing. Set
// HANGAR_VIEWER_BACKENDS to run on some backends only, and HANGAR_BINARY
// to test a prebuilt binary.
package viewer
