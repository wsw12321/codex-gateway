# CPA v8.0.48 Gateway — stage 5 candidate review

**Status: candidate committed for CI; not a production approval.** The active
compatibility lock now points at CPA tag `v8.0.48`, commit
`dbb706215a3afd2f0ea3c96482d4233856466caa` (`dbb7062`). The prior v8.0.30
source lock, patch and image remain the controlled rollback target.

## Artifacts

- Final patch: `deploy/codex-compat/cliproxy-v8.0.48-gateway.patch`
- Patch SHA-256: `325df2292a40cb27bc2753bf60d52d5b4c955ab0a839f0b3e381999dd0f6d30b`
- Upstream model catalog SHA-256:
  `32f663de911083339cf5347be74b0b4cbf1471bc1968362897cc94ebf021c306`
- Candidate image label:
  `codex-gateway-compat:v8.0.48-dbb70621-73ef6c2e8af5ecb0-cpa`

The final patch is the stage 4 combined artifact followed by the compile-closure
artifact. Both stages were generated against a clean CPA `dbb7062` checkout.

## Checks performed

- `git apply --check` and `git apply` succeeded on a clean CPA `dbb7062` checkout.
- The applied tree passed `git diff --check`; patch reverse-check also passed.
- `source.lock.json` parses with `jq`; the patch SHA matches the lock.
- Compose YAML parses and carries the v8.0.48 tag, full commit and patch SHA.
- Shell syntax checks pass for `scripts/validate-compose.sh` and
  `scripts/test-sidecar-image.sh`.

A real Go toolchain is required for `gofmt`, `go test`, `go test -race`, `go vet`
and `go build`; this execution environment exposes the desktop `/usr/bin/go`
board-game binary instead. CI must complete those checks, the Docker image build,
security jobs and real-account acceptance before production cutover.
