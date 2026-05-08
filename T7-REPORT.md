# T7 Sprint 2 / R2 — S3Store production wiring

Branch: `feat/sprint2-r2-s3store`
Base: `dd7a119f` (test(catalog): F6 follow-up coverage uplift)

## What changed

Replaced the `internal/asset.S3Store` skeleton stub (which only ever returned `ErrStorageNotConfigured`) with a real, production-grade Cloudflare R2 / S3-compatible client built on `aws-sdk-go-v2`.

### Files

| File | Change |
|------|--------|
| `internal/asset/store.go` | Removed the `S3Store` stub (struct + stub `Put`). Updated package doc to reference `s3_store.go`. `LocalDiskStore` and `validateObjectKey` are untouched. |
| `internal/asset/s3_store.go` | **New.** Production R2/S3 client. `S3Config` + `NewS3Store` constructor, `Put`/`Exists`/`SignedURL` methods, internal mockable interfaces (`s3Uploader`, `s3HeadAPI`, `s3PresignAPI`), `isNotFound` helper that handles `*types.NotFound`, `*types.NoSuchKey`, and the smithy 404 fallback, `wrapAWSError` to keep AWS SDK type names out of caller-facing errors. |
| `internal/asset/s3_store_test.go` | **New.** Mock-based unit tests covering: required-field validation, R2 region defaulting to `auto`, public-URL fast path vs. presigned fallback, AWS error wrapping, key-traversal rejection, ctx cancellation, `Exists` true / false-on-NoSuchKey / false-on-NotFound / false-on-smithy-404 / non-404 propagation, presigner TTL defaulting, fail-fast on bare struct. Optional `TestS3Store_Integration` skipped unless `R2_INTEGRATION=1`. |
| `internal/asset/store_test.go` | Removed `TestS3Store_Stub_ReturnsNotConfigured` (its premise — that the stub returns `ErrStorageNotConfigured` — has been replaced by `TestS3Store_BareStruct_FailsFast` in `s3_store_test.go`, which exercises the same fail-fast contract on the real type). |
| `docs/DEPLOY.md` | Added "Object storage configuration" section: env-var table for R2 (`R2_ACCOUNT_ID`, `R2_ENDPOINT`, `R2_BUCKET`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_PUBLIC_URL_BASE`, `R2_REGION`, `R2_PRESIGN_TTL`), R2 quirks the client handles automatically (region=auto, path-style addressing, custom endpoint), and instructions for running `TestS3Store_Integration` against a real R2 bucket. |
| `go.mod` / `go.sum` | Added `aws-sdk-go-v2/feature/s3/manager v1.22.18` and `aws-sdk-go-v2/service/s3 v1.101.0` (and their transitive deps). Bumped `aws-sdk-go-v2` from `v1.37.2` to `v1.41.7`, `credentials` to `v1.19.16`, `smithy-go` to `v1.25.1`. |

### Constraints honored

- `LocalDiskStore` untouched — same `Storage` interface, same callsites, same behavior.
- No code touched outside `internal/asset/` and `docs/DEPLOY.md`. The asset coordinator (`worker.go`) and 60-second hosting window logic (`worker.go` + `downloader.go`) are unchanged.
- AP-13 streaming preserved: `Put` wires the `io.Reader` straight into `manager.Uploader` (5 MiB chunked), with a `countingReader` decorator to capture the authoritative byte count without buffering.
- No credentials in code or tests. `NewS3Store` validates required fields and returns `ErrStorageNotConfigured` (wrapping the missing field) on bad config. Unit tests mock all three AWS surfaces; CI never needs R2 access.
- AWS errors don't leak through. `wrapAWSError` strips smithy types in callers' `err.Error()`, but `errors.Is` / `errors.As` still work via `%w`.

## AWS SDK versions added

```
github.com/aws/aws-sdk-go-v2                v1.37.2  -> v1.41.7
github.com/aws/aws-sdk-go-v2/credentials    v1.17.11 -> v1.19.16
github.com/aws/aws-sdk-go-v2/feature/s3/manager      v1.22.18  (new)
github.com/aws/aws-sdk-go-v2/service/s3              v1.101.0  (new)
github.com/aws/smithy-go                    v1.22.5  -> v1.25.1
```

`aws-sdk-go-v2/service/bedrockruntime v1.33.0` (used by `internal/adapter`) is unaffected — bedrockruntime continues to track its existing version.

Note: AWS now considers `feature/s3/manager` superseded by `feature/s3/transfermanager`, but `manager` is what the team brief named, is still the production package, and `transfermanager` is unreleased v0.x. Future migration is a separate sprint.

## Env vars introduced

All optional from the codebase's POV — `S3Store` is constructed by the deployment layer, not by `internal/asset` directly:

- `R2_ACCOUNT_ID` (or `R2_ENDPOINT` for non-R2)
- `R2_BUCKET`
- `R2_ACCESS_KEY_ID`
- `R2_SECRET_ACCESS_KEY`
- `R2_PUBLIC_URL_BASE` (optional; toggles public-URL fast path vs. presigned URL)
- `R2_REGION` (optional; defaults to `auto`)
- `R2_PRESIGN_TTL` (optional; defaults to 15 minutes)

Wiring these into the deployment layer's `S3Config` is left for the deploy-time integration step (out of scope for T7 per "do NOT modify code outside `internal/asset/`"). Existing `S3_*` env vars in `docker-compose.yaml` (MinIO) continue to work unchanged for now and can be rewired through the same `S3Config` (`Endpoint`, `Bucket`, `AccessKeyID`, `SecretAccessKey`) when the deploy step lands.

## Coverage

| Package | Before | After |
|---------|--------|-------|
| `internal/asset` | 87.4% | **88.6%** |

Full repo-wide test pass: `go test ./internal/... -count=1 -short -cover` — all packages green (transient Windows Defender "access denied" on test binaries auto-resolves on retry; not from this change). No package outside `internal/asset` was touched, so other coverage numbers are unaffected.

## How to run the integration test (when you have R2 creds)

```bash
export R2_INTEGRATION=1
export R2_ACCOUNT_ID=...           # or set R2_ENDPOINT directly
export R2_ACCESS_KEY_ID=...
export R2_SECRET_ACCESS_KEY=...
export R2_BUCKET=modelhub-outputs-staging
export R2_PUBLIC_URL_BASE=https://cdn.modelhub.example/   # optional fast-path check

go test ./internal/asset/... -run TestS3Store_Integration -v -count=1
```

Test uploads `outputs/t7-integration/{unix_nano}.txt` and `HEAD`-checks it. Cleanup is manual (`aws s3 rm` or R2 dashboard) — the test does not delete to keep failure forensics intact.

## Commit

`feat(asset): R2 S3Store implementation using aws-sdk-go-v2`

SHA: <will be filled in by the commit step>
