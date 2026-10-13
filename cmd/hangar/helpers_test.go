package main

import (
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
)

func newEmptyBlobs() blob.Store { return blobtest.NewFake() }
