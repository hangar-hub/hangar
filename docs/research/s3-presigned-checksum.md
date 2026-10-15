# Presigned S3 PUTs with a signed SHA-256 checksum

Research date: 2026-10-13. Asked for by [04](../spec/04-git.md) "Batch": "Sign `x-amz-checksum-sha256` into it when the store supports it (W1 tests this against AWS and MinIO, and records the result in `docs/research/`)". It also settles two items that [`git-server-research.md`](git-server-research.md) "Risks" lists as unverified: presigned PUTs with `x-amz-checksum-sha256`, and the minio-go presigned part upload round trip.

Evidence labels:

- **[test]**: run on this machine with the test named, against `pgsty/minio:RELEASE.2026-08-04T00-00-00Z` started by testcontainers, through `internal/blob/s3` (minio-go v7.4.1).
- **[src]**: read in the source at the version given.
- **[doc]**: official documentation.
- **[untested]**: not run. Check before you depend on it.

## Findings

1. **minio-go signs the header into the URL.** `Client.PresignHeader(ctx, "PUT", bucket, key, ttl, nil, http.Header{"x-amz-checksum-sha256": {...}})` adds the header to `X-Amz-SignedHeaders`, so the client must send it with the same value. [src] minio-go v7.4.1 `api-presigned.go` (`presignURL` with `extraPresignHeader`); [test] `TestPresignPutLimit` checks the URL names the header.
2. **The value is base64 of the 32 SHA-256 bytes**, not hex. An LFS `oid` is hex, so convert it. [test] `TestPresignPutLimit`; the checksum of `x` is `LXEWQrcmsEQBYnyp+6wy9chTD7GQPMTbAiWHF5IaSIE=`.
3. **MinIO verifies the body against the signed checksum.** A PUT whose body has a different SHA-256 gets `400 XAmzContentChecksumMismatch` ("The provided 'x-amz-checksum' header does not match what was computed.") and stores nothing. [test] conformance subtest `PresignPutHTTP` (`ChecksumPresign: true`) on MinIO, plus a one-off probe of the error body.
4. **Leaving the signed header out is refused.** MinIO answers `400 AccessDenied` ("There were headers present in the request which were not signed"). Any client that does not send the header cannot use such a URL. [test] one-off probe.
5. **A matching body succeeds** with `200`, and the object reads back unchanged. [test] `PresignPutHTTP`.
6. **Presigned multipart parts round-trip.** `Client.Presign(ctx, "PUT", bucket, key, ttl, {partNumber, uploadId})` URLs accept part bytes from a plain HTTP client; the `ETag` response headers complete the upload. [test] conformance subtest `PresignPartHTTP` on MinIO.
7. **Presigned GETs carry `response-content-disposition`** and honour `Range`, so `hf_hub_download` resumes work through a redirect. [test] conformance subtest `PresignGetHTTP`.

## What clients send

- **`huggingface_hub` sends no headers on a `basic` upload.** `_upload_single_part` calls `http_backoff("PUT", upload_url, data=fileobj)` and ignores `actions.upload.header` unless it has `chunk_size` (the multipart path, whose part PUTs also send no headers). [src] `huggingface_hub` 1.24.0 `lfs.py`, `lfs_upload` and `_upload_single_part`.
- **git-lfs applies the action's `header` map** to the request. [doc] git-lfs `docs/api/batch.md` (main, fetched 2026-10-13), on every action: "Optional hash of String HTTP header key/value pairs to apply to the request." `docs/api/basic-transfers.md` shows an upload action's `Authorization` header sent on the PUT. Not run against git-lfs 3.7.1 here [untested].

## Recommendation for the LFS batch API (W3) and Hub uploads (W4)

- Do **not** sign `x-amz-checksum-sha256` into `basic` upload URLs answered to Hugging Face clients: by finding 4, every `huggingface_hub` upload would fail. Use `blob.Store.PresignPut(ctx, key, size, nil, ttl)` and rely on the `verify` action, where `repo.LFSStore.Verify` re-hashes the object by streaming it (04 "Verify").
- For git-lfs clients, signing is safe and gives a server-side check: pass the oid's bytes as `sum` and copy `PresignedRequest.Headers` into `actions.upload.header`. Telling the two clients apart needs the batch request's `User-Agent` (`git-lfs/...`); if that is not worth the branching, leave the checksum out for everyone.
- `blob.PutVerified` covers uploads that pass through Hangar on any backend: the object becomes visible only when its SHA-256 matches.

## Not tested

- Other S3-compatible stores (Ceph RGW, R2, B2, Garage) [untested]. Stores that ignore the checksum would accept a corrupt body, which `verify` still catches.

## AWS S3

Run on 2026-10-15 from a Mac on a home connection (about 30 Mbit/s up) against throwaway private buckets in `eu-west-2`, with the public access block on, deleted after each run. minio-go v7.4.1 turns on the dual-stack endpoint for AWS by default, so requests went to `s3.dualstack.eu-west-2.amazonaws.com`. Command:

```sh
HANGAR_TEST_S3_ENDPOINT=https://s3.eu-west-2.amazonaws.com HANGAR_TEST_S3_ACCESS_KEY=... HANGAR_TEST_S3_SECRET_KEY=... \
HANGAR_TEST_S3_BUCKET=<bucket> HANGAR_TEST_S3_REGION=eu-west-2 go test -count=1 -timeout 90m -v ./internal/blob/s3/
```

- **The whole S3 suite passes on AWS**, twice: every conformance subtest, `TestCheck`, `TestStreamsLargeObjects` (48 MiB, peak heap growth 2 MiB or less), `TestStoredContentTypeIsPinned` and the presign tests. [test]
- **Findings 3, 5, 6 and 7 hold on AWS**: a signed checksum rejects a corrupt body, a matching body succeeds, presigned parts round-trip and presigned GETs carry the forced `Content-Type` and `Content-Disposition`. [test] conformance subtests `PresignPutHTTP`, `PresignPartHTTP`, `PresignGetHTTP`, `PresignedDownloadsAreAttachments`.
- **AWS answers differ from MinIO's.** A one-off probe against a URL with `Content-Type` and `x-amz-checksum-sha256` signed in got: [test]
  - a corrupt body: `400 BadDigest` ("The SHA256 you specified did not match the calculated checksum."); MinIO answers `400 XAmzContentChecksumMismatch`.
  - no headers, as `huggingface_hub` sends on a `basic` upload, or the type without the checksum: `403 SignatureDoesNotMatch`; MinIO answers `400 AccessDenied` ("There were headers present in the request which were not signed"). Finding 4 holds on AWS: a client that leaves out a signed header cannot use the URL.
  - another `Content-Type`: `403 SignatureDoesNotMatch` on both.
- **Concurrent PUTs to one key get `503 SlowDown`** on AWS (subtest `ConcurrentPuts`), and the store's upload retries recover them. [test]

### First run, 2026-10-14, and what changed

The first run, on 2026-10-14 over the same connection while it was busier, passed the checksum, part and GET presign tests and failed four others. Both causes were in Hangar, not AWS:

- `TestCheck`: AWS answers `HEAD` for an object in a missing bucket with a bare `404`, the same as for a missing object; MinIO adds an `X-Minio-Error-Code: NoSuchBucket` header. `Stat` reported a missing object. `Stat` now confirms the bucket with `HeadBucket` when an object is missing, at most once a minute while the bucket exists, and `Check` explains a missing bucket, a bucket in another region (from `x-amz-bucket-region` on a `301`) and a `403`.
- `PutVerifiedMultipart`, `MultipartPartRetryWithNewContent` and `TestStreamsLargeObjects` failed on single part PUTs with "Connection closed by foreign host" (the connection closed before a response), "net/http: timeout awaiting response headers" and "use of closed network connection". minio-go retries a request only when its body is an `io.Seeker` (`executeMethod`, `api.go`), and Hangar wrapped every part body in non-seekable readers, so one dropped connection failed the whole upload. minio-go's transport also waits only one minute for response headers, a clock that starts when the last body bytes reach the socket buffer, not the server. [src] minio-go v7.4.1 `api.go` and `transport.go`.

The store now retries part and single PUTs itself (`storage.s3.retries`, 5 by default, backing off from 1 to 30 seconds), cancels and retries a request that sends nothing and gets no answer for `storage.s3.stall_timeout` (2 minutes), and uses that timeout for response headers. A body it cannot rewind is copied to a temporary file as it is sent, so a retry replays the same bytes and the source is still read once. The fault-injection tests in `internal/blob/s3` (`TestPartUploadRetriesFaults`, `TestPutVerifiedMultipartSurvivesFaults`, `TestPartRetriesOnMinIO`) reproduce a reset connection, a stall in the middle of the body, no response and `SlowDown`. The busy-link conditions of the first run did not recur on 2026-10-15, so the retries on AWS were seen only for `SlowDown`; the dual-stack endpoint is unchanged, and nothing showed it to be a cause.
