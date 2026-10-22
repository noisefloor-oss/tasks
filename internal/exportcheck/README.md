# Source export checks

Run `go test -count=1 -timeout 20m ./internal/exportcheck` after committing.
The integration checks inspect `git archive HEAD`, so uncommitted changes
are not part of their input. In an extracted source archive without Git
metadata, they skip; the scanner's regression tests still run.

The content check uses Git's committed export attributes to reject excluded
members and references to excluded paths. It scans every UTF-8 text member
for private vocabulary and checks relative Markdown destinations against
archive members. Inline links, images, reference links, and directory links
are checked; code examples are ignored. URL fragments and remote destinations
are not validated.

The build check extracts the archive into a temporary directory and runs
`go build ./...` and `go vet ./...` with workspace mode disabled. It never
executes the archived test suite recursively.

`LICENSE`, `README.md`, `SECURITY.md`, `DEVELOPMENT.md`, and
`.github/ISSUE_TEMPLATE/config.yml` are all required. `pendingPublicFiles` is
empty, so no missing files are exempted. Present files are always scanned
for private content and broken links. `requiredExcludedFiles` pins internal
paths whose export-ignore rules must not be removed; a dropped rule fails
the content check.

`allowedHits` records a path, matched-text pattern, and reason for every
exception. Synthetic task titles, fixture assertions, and comments describing
those fixtures are domain data. The packaging script's refused install prefix
is part of the setuid broker safety rule. Verbose test output lists every
exception for review. Add exceptions only for legitimate domain data, never
to grandfather private prose.

Private-pattern literals are assembled from fragments, including regression
examples, so the checker can scan its own source without a file exemption.
