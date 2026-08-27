## What changed

<!-- Describe the user-visible change and why it belongs in unring. -->

## Safety boundary

<!-- What can this code fail to intercept, restore, roll back, or compensate? -->

## Verification

- [ ] `gofmt -l .` is empty
- [ ] `go vet ./...`
- [ ] `go build ./...`
- [ ] `go test ./...`
- [ ] Integration tests were run, or the reason they were not is explained below
- [ ] User-facing coverage gaps and undo limits are explicit
- [ ] No LLM was added to the classification path
- [ ] One commit/discard decision still applies to the whole session

## Evidence

<!-- Paste concise test output, screenshots, or a reproduction. Redact secrets. -->
