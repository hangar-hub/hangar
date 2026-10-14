# Upstream Pull compatibility suite

Go tests with the `compat` build tag for Pulls from Upstreams (docs/spec/13-testing.md "Upstream pulls"). Run them with:

```sh
go test -count=1 -tags compat ./compat/upstream/
```

| Test | What it checks | Upstream |
|---|---|---|
| `TestPullTinyRepoFromHuggingFace` | A whole Repo pulled: the Mirror's `main` is the Hub's `sha`, every file is there, every LFS object matches its SHA-256, and `git fsck` passes on the Mirror's history. | huggingface.co: `hf-internal-testing/tiny-random-bert` (28 MB) |
| `TestPartialMirrorOfOneQuantFromHuggingFace` | A Partial mirror of the `Q4_0` quant: only `stories15M-q4_0.gguf` is in the arsenal. | huggingface.co: `ggml-org/tiny-llamas` (19 MB transferred) |
| `TestPullThroughFromHuggingFace` | Pull-through: a missing Repo gets its history, and a file that is not in the arsenal streams while it arrives, then is stored. | huggingface.co: `hf-internal-testing/tiny-random-bert` |
| `TestTrackingPicksUpANewCommitFromASecondInstance` | Tracking: a `mirror.sync` run picks up a new upstream Commit and its new LFS object. | A second Instance the test controls |
| `TestBandwidthCapAcrossTwoWorkers` | Two workers pulling at once under one budget stay within 10 % of the cap over 60 seconds. | A second Instance |
| `TestResumeAfterKillingTheWorkerMidFile` | A worker stopped in the middle of a multipart transfer, and a new worker process that resumes it with an HTTP Range after the parts already uploaded. | A second Instance |

The huggingface.co tests need the network. Set `HF_TOKEN` to send a token (the fixtures are public, so none is needed). Set `HANGAR_UPSTREAM_CAP_SECONDS` to shorten the bandwidth window for a quick local run; CI uses the default of 60.

The fake scenarios run on `upstreamtest.Harness`: the real `upstream.Service` and HTTP client over the test doubles of the Repo service, Job queue and limiter, and `upstreamtest.Server` as the second Instance (a Hub API and `git http-backend` over real git repositories). `real_test.go` runs the same scenarios with the real `internal/repo`, `internal/jobs` worker and `jobs.DBLimiter`, and with the git server and Hub API as the second Instance. See `docs/build/handoffs/w6.md`.
