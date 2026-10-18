package main

import (
	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/blob/blobtest"
)

func newEmptyBlobs() blob.Store { return blobtest.NewFake() }
