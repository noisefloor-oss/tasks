package store

// §8.6 names inside the <common-dir>/noise-tasks store directory. The
// historic `sqlite-migration.json` receipts of the retired v1.4.0 migration
// engine are inert (SPEC §8.7): they are never read as v2.5 authority,
// journals, or receipts and never select or replace a database. The current
// §8.6 artifacts are PlanFile (plan), LegacyFile (source), StoreFile
// (target), and MigrationJournalFile (apply journal).

// LegacyFile is the retired JSON live store.
const LegacyFile = "live.json"

// ValidDigest reports whether v is a lowercase `sha256:<hex>` digest, the
// form every accepted plan/bundle/journal digest flag uses.
func ValidDigest(v string) bool { return digestForm(v) }
