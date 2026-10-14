//go:build compat

// Package viewer is the compat/viewer suite (docs/spec/14-datasets.md
// "Tests"): it converts small public datasets in Hangar and compares the
// Dataset Viewer's responses with the Hub's for the same Revision, checks
// Config and Split resolution against the `datasets` library, and
// validates Croissant output with mlcroissant. Run it with
// `go test -tags compat ./compat/viewer`.
package viewer
