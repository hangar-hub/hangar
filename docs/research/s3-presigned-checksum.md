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

- **AWS S3** [untested]. No AWS credentials were available. The AWS user guide documents `x-amz-checksum-sha256` on `PutObject` and rejection of a mismatched body [doc] ("Checking object integrity in Amazon S3", AWS S3 User Guide), but a presigned URL with the header signed in has not been run against AWS. Run `HANGAR_TEST_S3_ENDPOINT=https://s3.<region>.amazonaws.com HANGAR_TEST_S3_ACCESS_KEY=... HANGAR_TEST_S3_SECRET_KEY=... HANGAR_TEST_S3_BUCKET=... HANGAR_TEST_S3_REGION=<region> go test ./internal/blob/s3/` to check; the conformance suite asserts findings 3, 5, 6 and 7.
- Other S3-compatible stores (Ceph RGW, R2, B2, Garage) [untested]. Stores that ignore the checksum would accept a corrupt body, which `verify` still catches.
