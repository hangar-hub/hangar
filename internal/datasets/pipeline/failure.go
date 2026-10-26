package pipeline

import (
	"errors"

	"github.com/hangar-hub/hangar/internal/datasets/layout"
)

// Failure codes are what SplitRow.Error and StatsDoc.Error hold: the Hub's
// error code for why a dataset, Config or Split has no Parquet export or
// statistics. The error behind a failure is logged, never stored, because
// its text can name internal paths, services or other Repos' data.
const (
	// FailureGeneration means converting a Config failed.
	FailureGeneration = "DatasetGenerationError"
	// FailureEmpty means the dataset has no data files Hangar can read.
	FailureEmpty = "EmptyDatasetError"
	// FailureFormatMismatch means the Splits have data files of different
	// formats.
	FailureFormatMismatch = "FileFormatMismatchBetweenSplitsError"
	// FailureConfigNames means the card or the data files do not give
	// valid Configs.
	FailureConfigNames = "ConfigNamesError"
	// FailureStatistics means computing a Split's statistics failed.
	FailureStatistics = "UnexpectedError"
	// FailureNotPulled means a Partial mirror's Parquet export of a Split
	// is not in the arsenal. The Hub has no such case.
	FailureNotPulled = "ParquetNotPulledError"
)

// errNotPulled means a file of a mirrored Parquet export is not present.
var errNotPulled = errors.New("the Parquet export is not in the Arsenal")

// failureMessages are the Hub's messages for each failure code.
var failureMessages = map[string]string{
	FailureGeneration:     "The dataset generation failed",
	FailureEmpty:          "The dataset is empty.",
	FailureFormatMismatch: "Couldn't infer the same data file format for all splits.",
	FailureConfigNames:    "Cannot get the config names for the dataset.",
	FailureStatistics:     "Unexpected error.",
	FailureNotPulled:      "The Parquet export of this Split is not in the Arsenal. Pull it to preview the split.",
}

// Failure is a stored failure: the Hub's error code and message.
type Failure struct {
	Code    string
	Message string
}

// FailureOf returns the failure a stored code names. Rows written before
// codes were stored hold error text instead; they read as a failed
// conversion, so that text is never shown.
func FailureOf(stored string) Failure {
	if msg, ok := failureMessages[stored]; ok {
		return Failure{Code: stored, Message: msg}
	}
	return Failure{Code: FailureGeneration, Message: failureMessages[FailureGeneration]}
}

// LayoutFailure returns the failure code for an error resolving a
// dataset's Configs, and false when the error is not about the dataset's
// files or card (reading them failed), so the dataset has not failed.
func LayoutFailure(err error) (string, bool) {
	switch {
	case !layout.IsLayoutError(err):
		return "", false
	case errors.Is(err, layout.ErrEmptyDataset):
		return FailureEmpty, true
	case errors.Is(err, layout.ErrMixedFormats):
		return FailureFormatMismatch, true
	}
	return FailureConfigNames, true
}
