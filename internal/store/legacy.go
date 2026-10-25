package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// The §8.5 legacy backup surface: `backup create --legacy` reads only the
// exact derived legacy live.json, fully validates it with the byte-faithful
// §1 semantics, imports it into a temporary exact-v3 SQLite payload
// that is never published as tasks.sqlite, and proves the import against a
// source-side comparison digest before the bundle exists.

const (
	// legacyFamilyDomain prefixes the deterministic legacy family UUID
	// derivation (SPEC §8.5).
	legacyFamilyDomain = "noise-tasks-legacy-family-v1"
	// LegacySourceSchema is the literal sourceSchema of a legacy bundle.
	LegacySourceSchema = "legacy-json:1"
	// legacyMaxBytes bounds the legacy document (the v1 reader's bound).
	legacyMaxBytes = 128 << 20
	// legacyStoreSchemaVersion is the only legacy document version.
	legacyStoreSchemaVersion = 1
)

// LegacyFamilyUUID derives the deterministic §8.5 family UUID for a legacy
// repository family: the first 128 bits of SHA-256 over the domain, a NUL,
// the canonical Git common-directory path, a NUL, and the canonical
// owner-boundary path, with RFC 4122 version-5 and variant bits set.
// Repeated backups of one family are stable even when task content changes.
func LegacyFamilyUUID(commonDir, ownerBoundary string) string {
	h := sha256.New()
	h.Write([]byte(legacyFamilyDomain))
	h.Write([]byte{0})
	h.Write([]byte(commonDir))
	h.Write([]byte{0})
	h.Write([]byte(ownerBoundary))
	sum := h.Sum(nil)
	var id uuid.UUID
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x50 // RFC 4122 version 5
	id[8] = (id[8] & 0x3f) | 0x80 // RFC 4122 variant
	return id.String()
}

// legacyTagSnapshot is one tag's imported logical state: the tag row shape
// plus its task rows, in payload record form.
type legacyTagSnapshot struct {
	name           string
	nextID         int64
	taskCount      int64
	completedCount int64
	milestones     json.RawMessage
	extra          map[string]json.RawMessage
	tasks          []*Task
}

// legacySnapshot is the validated legacy document in payload record form.
type legacySnapshot struct {
	fencing int64
	nextSeq int64
	tags    map[string]*legacyTagSnapshot
	tagName []string // tag name byte order
	events  []Event
}

// LegacyRefusalError reports a fail-closed legacy backup refusal (exit 1).
type LegacyRefusalError struct{ Reason string }

func (e *LegacyRefusalError) Error() string { return e.Reason }

func legacyRefusalf(format string, args ...any) error {
	return &LegacyRefusalError{Reason: fmt.Sprintf(format, args...)}
}

// createLegacyCompatibilityDB creates the exact schema-v3 payload required
// by §8.5. It deliberately does not use Create: that constructor owns the
// current live-authority schema, while a legacy compatibility bundle must
// remain byte-for-byte verifiable by released v3-aware consumers.
func createLegacyCompatibilityDB(path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create legacy compatibility payload %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(abs)
		return nil, err
	}
	dsn := "file:" + abs + "?_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		os.Remove(abs)
		return nil, err
	}
	db := &DB{db: sqlDB, path: abs}
	cleanup := func(err error) (*DB, error) {
		sqlDB.Close()
		os.Remove(abs)
		return nil, err
	}
	if err := db.enableWAL(); err != nil {
		return cleanup(err)
	}
	if err := db.transact("BEGIN IMMEDIATE", func(t *tx) error {
		if _, err := t.Exec(schemaV2DDL + orchestratorSchemaDDL); err != nil {
			return fmt.Errorf("bootstrap legacy compatibility schema: %w", err)
		}
		if _, err := t.Exec(fmt.Sprintf("PRAGMA user_version=%d", orchestratorSchemaVersion)); err != nil {
			return fmt.Errorf("set legacy compatibility schema version: %w", err)
		}
		if _, err := t.Exec(`INSERT INTO meta(key, value) VALUES ('fencing', '0')`); err != nil {
			return fmt.Errorf("bootstrap legacy compatibility fencing counter: %w", err)
		}
		return nil
	}); err != nil {
		return cleanup(err)
	}
	return db, nil
}

// CreateLegacyBackup builds one §8.5 bundle from the exact derived legacy
// live.json at legacyPath. It never opens, creates, or publishes the current
// SQLite authority; the payload is a temporary database whose family identity
// is the deterministic §8.5 derivation. createdBy is the binary identity
// string. The source-side comparison digest and shared inventory are computed
// from the exact logical legacy content and must equal the imported payload's
// before the bundle is published (fail closed otherwise).
func CreateLegacyBackup(outputPath, legacyPath, commonDir, ownerBoundary, createdBy string) (*Manifest, error) {
	doc, err := readLegacyDocument(legacyPath)
	if err != nil {
		return nil, err
	}
	snap, err := buildLegacySnapshot(doc)
	if err != nil {
		return nil, err
	}
	sourceComparison, sourceInv, err := legacyComparison(snap)
	if err != nil {
		return nil, err
	}
	familyID := LegacyFamilyUUID(commonDir, ownerBoundary)

	payloadDir, err := os.MkdirTemp("", "noise-tasks-legacy-payload-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary legacy payload directory: %w", err)
	}
	defer os.RemoveAll(payloadDir)
	db, err := createLegacyCompatibilityDB(filepath.Join(payloadDir, PayloadFile))
	if err != nil {
		return nil, err
	}
	manifest, buildErr := publishBundle(outputPath, func(payloadPath string) (*Manifest, error) {
		if err := importLegacySnapshot(db, snap, familyID, ownerBoundary); err != nil {
			return nil, err
		}
		if err := db.IntegrityCheck(); err != nil {
			return nil, err
		}
		if err := db.ForeignKeyCheck(); err != nil {
			return nil, err
		}
		// The payload member is a fresh VACUUM INTO image of the temporary
		// database — never a byte copy, and never the authority path.
		if err := db.BackupTo(payloadPath); err != nil {
			return nil, err
		}
		payload, err := openUnlocked(payloadPath, false, "")
		if err != nil {
			return nil, err
		}
		defer payload.Close()
		if err := payload.IntegrityCheck(); err != nil {
			return nil, err
		}
		if err := payload.ForeignKeyCheck(); err != nil {
			return nil, err
		}
		if err := payload.validateSchemaVersion(orchestratorSchemaVersion); err != nil {
			return nil, err
		}
		semantic, comparison, inv, err := payload.ClassDigests()
		if err != nil {
			return nil, err
		}
		// The proof: the imported payload must equal the exact logical
		// legacy content on every shared class. No equality, no bundle.
		if comparison != sourceComparison {
			return nil, legacyRefusalf("legacy import proof failed: source comparison digest %s, payload %s", sourceComparison, comparison)
		}
		if *inv != *sourceInv {
			return nil, legacyRefusalf("legacy import proof failed: source inventory %+v, payload %+v", *sourceInv, *inv)
		}
		version, err := payload.userVersion()
		if err != nil {
			return nil, err
		}
		if version != orchestratorSchemaVersion {
			return nil, fmt.Errorf("legacy payload schema %d, want %d", version, orchestratorSchemaVersion)
		}
		payloadDigest, err := hashFile(payloadPath)
		if err != nil {
			return nil, err
		}
		return &Manifest{
			SchemaVersion:           ManifestSchemaVersion,
			CreatedBy:               createdBy,
			CreatedAt:               nowUTC(),
			FamilyID:                familyID,
			SourceKind:              SourceKindLegacyJSON,
			SourceSchema:            LegacySourceSchema,
			CurrentSchemaVersion:    orchestratorSchemaVersion,
			Inventory:               *inv,
			PayloadDigest:           payloadDigest,
			SemanticDigestVersion:   SemanticDigestVersion,
			SemanticDigest:          semantic,
			ComparisonDigestVersion: ComparisonDigestVersion,
			ComparisonDigest:        comparison,
		}, nil
	})
	db.Close()
	return manifest, buildErr
}

// --- legacy document parsing ---

// legacyDocument is the raw parsed v1 live store document.
type legacyDocument struct {
	fencing int64
	nextSeq int64
	tags    map[string]map[string]json.RawMessage
	events  []map[string]json.RawMessage
}

// readLegacyDocument loads and structurally validates the exact legacy
// document. A missing, irregular, symlinked, oversized, or malformed document
// fails closed.
func readLegacyDocument(path string) (*legacyDocument, error) {
	// The document is opened once with O_NOFOLLOW and read through that
	// handle: regularity and size are validated from the open file, never a
	// re-stat of a pathname that could have been swapped for a symlink.
	f, info, err := openRegularNoFollow(path)
	if err != nil {
		if errors.Is(err, errNotRegular) {
			return nil, legacyRefusalf("legacy live store %s is not a regular file", path)
		}
		return nil, fmt.Errorf("read legacy live store %s: %w", path, err)
	}
	defer f.Close()
	if info.Size() > legacyMaxBytes {
		return nil, legacyRefusalf("legacy live store %s exceeds %d bytes", path, legacyMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, legacyMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read legacy live store: %w", err)
	}
	if len(data) > legacyMaxBytes {
		return nil, legacyRefusalf("legacy live store %s exceeds %d bytes", path, legacyMaxBytes)
	}
	return parseLegacyDocument(data)
}

func parseLegacyDocument(data []byte) (*legacyDocument, error) {
	// Full validation includes strict JSON: duplicate object keys at any
	// level are ambiguous and refused before any byte-faithful field is read.
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, legacyRefusalf("legacy live store is not strict JSON: %v", err)
	}

	top, err := rawObject(json.RawMessage(data), "legacy store")
	if err != nil {
		return nil, err
	}
	if err := requireKeys(top, "legacy store",
		[]string{"schemaVersion", "fencing", "nextSeq", "base", "tags", "events"}); err != nil {
		return nil, err
	}
	var schemaVersion int64
	if err := decodeLegacyInt(top["schemaVersion"], &schemaVersion); err != nil {
		return nil, fmt.Errorf("legacy schemaVersion: %w", err)
	}
	if schemaVersion != legacyStoreSchemaVersion {
		return nil, legacyRefusalf("unsupported legacy schema version %d", schemaVersion)
	}
	doc := &legacyDocument{tags: map[string]map[string]json.RawMessage{}}
	if err := decodeLegacyInt(top["fencing"], &doc.fencing); err != nil {
		return nil, fmt.Errorf("legacy fencing counter: %w", err)
	}
	if doc.fencing < 0 {
		return nil, legacyRefusalf("invalid legacy fencing counter %d", doc.fencing)
	}
	var nextSeq int64
	if err := decodeLegacyInt(top["nextSeq"], &nextSeq); err != nil {
		return nil, fmt.Errorf("legacy nextSeq: %w", err)
	}
	if nextSeq < 1 {
		return nil, legacyRefusalf("invalid legacy nextSeq %d", nextSeq)
	}
	doc.nextSeq = nextSeq
	// The projection base and its snapshots are synchronization debris
	// (SPEC §8.6 step 1); only their shape is validated, never migrated.
	base, err := rawObject(top["base"], "legacy base")
	if err != nil {
		return nil, err
	}
	if err := requireKeys(base, "legacy base",
		[]string{"hash", "tags"}, "milestones"); err != nil {
		return nil, err
	}
	var baseHash string
	if err := json.Unmarshal(base["hash"], &baseHash); err != nil {
		return nil, fmt.Errorf("legacy base hash: %w", err)
	}
	if len(baseHash) != 64 {
		return nil, legacyRefusalf("invalid legacy base hash %q", baseHash)
	}
	if _, err := hex.DecodeString(baseHash); err != nil {
		return nil, legacyRefusalf("invalid legacy base hash %q", baseHash)
	}
	tagsRaw := top["tags"]
	if isJSONNullRaw(tagsRaw) {
		return nil, legacyRefusalf("legacy store is missing \"tags\"")
	}
	var tagBlocks map[string]json.RawMessage
	if err := json.Unmarshal(tagsRaw, &tagBlocks); err != nil {
		return nil, fmt.Errorf("legacy tags: %w", err)
	}
	if tagBlocks == nil {
		return nil, legacyRefusalf("legacy store is missing \"tags\"")
	}
	for name, block := range tagBlocks {
		if name == "" {
			return nil, legacyRefusalf("legacy tag name is empty")
		}
		obj, err := rawObject(block, fmt.Sprintf("legacy tag %q", name))
		if err != nil {
			return nil, err
		}
		doc.tags[name] = obj
	}
	eventsRaw := top["events"]
	if isJSONNullRaw(eventsRaw) {
		return nil, legacyRefusalf("legacy store is missing \"events\"")
	}
	var eventList []json.RawMessage
	if err := json.Unmarshal(eventsRaw, &eventList); err != nil {
		return nil, fmt.Errorf("legacy events: %w", err)
	}
	if eventList == nil {
		return nil, legacyRefusalf("legacy store is missing \"events\"")
	}
	doc.events = make([]map[string]json.RawMessage, len(eventList))
	for i, raw := range eventList {
		obj, err := rawObject(raw, fmt.Sprintf("legacy event %d", i))
		if err != nil {
			return nil, err
		}
		doc.events[i] = obj
	}
	return doc, nil
}

// buildLegacySnapshot converts the parsed document into payload record form,
// applying the byte-faithful §1 task semantics and the graph and invariant
// checks a valid legacy store must satisfy.
func buildLegacySnapshot(doc *legacyDocument) (*legacySnapshot, error) {
	snap := &legacySnapshot{fencing: doc.fencing, nextSeq: doc.nextSeq, tags: map[string]*legacyTagSnapshot{}}
	for name := range doc.tags {
		snap.tagName = append(snap.tagName, name)
	}
	sort.Strings(snap.tagName)
	for _, name := range snap.tagName {
		entry, err := buildLegacyTag(name, doc.tags[name])
		if err != nil {
			return nil, err
		}
		snap.tags[name] = entry
	}
	for i, obj := range doc.events {
		event, err := buildLegacyEvent(obj)
		if err != nil {
			return nil, fmt.Errorf("legacy event %d: %w", i, err)
		}
		snap.events = append(snap.events, *event)
	}
	// The retained log may be truncated; nextSeq still names the sequence the
	// next appended event must take, so it is strictly above every retained
	// seq (and positive, enforced at parse).
	for _, e := range snap.events {
		if snap.nextSeq <= e.Seq {
			return nil, legacyRefusalf("legacy nextSeq %d is not above retained event seq %d", snap.nextSeq, e.Seq)
		}
	}
	if err := validateLegacyGraph(snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func buildLegacyTag(name string, obj map[string]json.RawMessage) (*legacyTagSnapshot, error) {
	entry := &legacyTagSnapshot{name: name}
	known := map[string]bool{
		"nextId": true, "taskCount": true, "completedCount": true, "tasks": true, "milestones": true,
	}
	for key, raw := range obj {
		if !known[key] {
			if entry.extra == nil {
				entry.extra = map[string]json.RawMessage{}
			}
			entry.extra[key] = raw
		}
	}
	into := func(key string, dst *int64) error {
		raw, ok := obj[key]
		if !ok || isJSONNullRaw(raw) {
			return nil
		}
		if err := decodeLegacyInt(raw, dst); err != nil {
			return fmt.Errorf("legacy tag %q %s: %w", name, key, err)
		}
		return nil
	}
	if err := into("nextId", &entry.nextID); err != nil {
		return nil, err
	}
	if err := into("taskCount", &entry.taskCount); err != nil {
		return nil, err
	}
	if err := into("completedCount", &entry.completedCount); err != nil {
		return nil, err
	}
	if entry.nextID < 0 || entry.taskCount < 0 || entry.completedCount < 0 {
		return nil, legacyRefusalf("legacy tag %q has a negative counter", name)
	}
	if raw, ok := obj["milestones"]; ok && !isJSONNullRaw(raw) {
		// Full §12.1 validation before publication: each entry an object
		// with required name and status, the §12.1 name grammar, status
		// exactly open|closed, and exact-name uniqueness. Unknown entry
		// fields and the array order stay byte-faithful in the stored raw.
		if _, err := backlog.ParseMilestones(name, raw); err != nil {
			return nil, legacyRefusalf("legacy tag %q milestones registry: %v", name, err)
		}
		if len(raw) > 0 {
			entry.milestones = raw
		}
	}
	tasksRaw, ok := obj["tasks"]
	if !ok || isJSONNullRaw(tasksRaw) {
		tasksRaw = json.RawMessage(`[]`)
	}
	var records []json.RawMessage
	if err := json.Unmarshal(tasksRaw, &records); err != nil {
		return nil, fmt.Errorf("legacy tag %q tasks: %w", name, err)
	}
	entry.tasks = make([]*Task, len(records))
	for i, recRaw := range records {
		rec, err := rawObject(recRaw, fmt.Sprintf("legacy tag %q task %d", name, i))
		if err != nil {
			return nil, err
		}
		fields, ok := rec["task"]
		if !ok {
			return nil, legacyRefusalf("legacy tag %q task %d is missing \"task\"", name, i)
		}
		task, err := taskFromFields(name, fields)
		if err != nil {
			return nil, fmt.Errorf("legacy tag %q task %d: %w", name, i, err)
		}
		var revision int64
		rawRev, ok := rec["revision"]
		if !ok {
			return nil, legacyRefusalf("legacy tag %q task %d is missing \"revision\"", name, i)
		}
		if isJSONNullRaw(rawRev) {
			return nil, legacyRefusalf("legacy tag %q task %d has a null revision", name, i)
		}
		if err := decodeLegacyInt(rawRev, &revision); err != nil {
			return nil, fmt.Errorf("legacy tag %q task %d revision: %w", name, i, err)
		}
		if revision <= 0 {
			return nil, legacyRefusalf("legacy tag %q task %d has invalid revision %d", name, i, revision)
		}
		task.Revision = int(revision)
		claim, err := buildLegacyClaim(name, i, rec["claim"])
		if err != nil {
			return nil, err
		}
		task.Claim = claim
		binding, err := buildLegacyBinding(name, i, rec["worktree"])
		if err != nil {
			return nil, err
		}
		task.Binding = binding
		// Unknown fields of the outer record — siblings of task, revision,
		// claim, and worktree — are preserved verbatim like every other
		// unknown field (§1 round-trip rule).
		if err := collectExtra(rec, legacyRecordKnownKeys, &task.OuterExtra); err != nil {
			return nil, err
		}
		entry.tasks[i] = task
	}
	return entry, nil
}

func buildLegacyClaim(tag string, index int, raw json.RawMessage) (*Claim, error) {
	if len(raw) == 0 || isJSONNullRaw(raw) {
		return nil, nil
	}
	obj, err := rawObject(raw, fmt.Sprintf("legacy tag %q task %d claim", tag, index))
	if err != nil {
		return nil, err
	}
	claim := &Claim{}
	str := func(key string, dst *string) error {
		v, ok := obj[key]
		if !ok || isJSONNullRaw(v) {
			return nil
		}
		if err := json.Unmarshal(v, dst); err != nil {
			return fmt.Errorf("legacy claim %s: %w", key, err)
		}
		return nil
	}
	if err := str("owner", &claim.Owner); err != nil {
		return nil, err
	}
	if err := str("acquiredAt", &claim.AcquiredAt); err != nil {
		return nil, err
	}
	if err := str("expiresAt", &claim.ExpiresAt); err != nil {
		return nil, err
	}
	var token int64
	if v, ok := obj["token"]; ok && !isJSONNullRaw(v) {
		if err := decodeLegacyInt(v, &token); err != nil {
			return nil, fmt.Errorf("legacy claim token: %w", err)
		}
	}
	claim.Token = token
	var ttl int64
	if v, ok := obj["ttl"]; ok && !isJSONNullRaw(v) {
		if err := decodeLegacyInt(v, &ttl); err != nil {
			return nil, fmt.Errorf("legacy claim ttl: %w", err)
		}
	}
	claim.TTL = int(ttl)
	if err := collectExtra(obj, claimKnownKeys, &claim.Extra); err != nil {
		return nil, err
	}
	if claim.Owner == "" || claim.TTL <= 0 || claim.AcquiredAt == "" || claim.ExpiresAt == "" {
		return nil, legacyRefusalf("legacy tag %q task %d has an invalid claim", tag, index)
	}
	return claim, nil
}

func buildLegacyBinding(tag string, index int, raw json.RawMessage) (*WorktreeBinding, error) {
	if len(raw) == 0 || isJSONNullRaw(raw) {
		return nil, nil
	}
	obj, err := rawObject(raw, fmt.Sprintf("legacy tag %q task %d worktree", tag, index))
	if err != nil {
		return nil, err
	}
	binding := &WorktreeBinding{}
	str := func(key string, dst *string) error {
		v, ok := obj[key]
		if !ok || isJSONNullRaw(v) {
			return nil
		}
		if err := json.Unmarshal(v, dst); err != nil {
			return fmt.Errorf("legacy worktree %s: %w", key, err)
		}
		return nil
	}
	if err := str("path", &binding.Path); err != nil {
		return nil, err
	}
	if err := str("boundary", &binding.Boundary); err != nil {
		return nil, err
	}
	if err := str("branch", &binding.Branch); err != nil {
		return nil, err
	}
	if err := str("createdAt", &binding.CreatedAt); err != nil {
		return nil, err
	}
	if v, ok := obj["keep"]; ok && !isJSONNullRaw(v) {
		if err := json.Unmarshal(v, &binding.Keep); err != nil {
			return nil, fmt.Errorf("legacy worktree keep: %w", err)
		}
	}
	if err := collectExtra(obj, worktreeKnownKeys, &binding.Extra); err != nil {
		return nil, err
	}
	return binding, nil
}

func buildLegacyEvent(obj map[string]json.RawMessage) (*Event, error) {
	event := &Event{}
	for _, key := range []string{"seq", "tag", "kind", "at"} {
		if _, ok := obj[key]; !ok {
			return nil, legacyRefusalf("legacy event is missing required key %q", key)
		}
	}
	str := func(key string, dst *string) error {
		v, ok := obj[key]
		if !ok || isJSONNullRaw(v) {
			return nil
		}
		if err := json.Unmarshal(v, dst); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		return nil
	}
	if err := str("task", &event.Task); err != nil {
		return nil, err
	}
	if err := str("tag", &event.Tag); err != nil {
		return nil, err
	}
	if err := str("kind", &event.Kind); err != nil {
		return nil, err
	}
	if err := str("status", &event.Status); err != nil {
		return nil, err
	}
	if err := str("owner", &event.Owner); err != nil {
		return nil, err
	}
	if err := str("action", &event.Action); err != nil {
		return nil, err
	}
	if err := str("at", &event.At); err != nil {
		return nil, err
	}
	var n int64
	if v, ok := obj["seq"]; ok && !isJSONNullRaw(v) {
		if err := decodeLegacyInt(v, &n); err != nil {
			return nil, fmt.Errorf("seq: %w", err)
		}
	}
	event.Seq = n
	if v, ok := obj["revision"]; ok && !isJSONNullRaw(v) {
		if err := decodeLegacyInt(v, &n); err != nil {
			return nil, fmt.Errorf("revision: %w", err)
		}
		event.Revision = int(n)
	}
	if v, ok := obj["count"]; ok && !isJSONNullRaw(v) {
		if err := decodeLegacyInt(v, &n); err != nil {
			return nil, fmt.Errorf("count: %w", err)
		}
		event.Count = int(n)
	}
	if err := collectExtra(obj, legacyEventKnown, &event.Extra); err != nil {
		return nil, err
	}
	if event.Seq < 1 {
		return nil, legacyRefusalf("event seq %d is not a positive integer", event.Seq)
	}
	if event.Kind == "" || event.At == "" {
		return nil, errors.New("event is missing \"kind\" or \"at\"")
	}
	return event, nil
}

var legacyRecordKnownKeys = map[string]bool{
	"task": true, "revision": true, "claim": true, "worktree": true,
}

var legacyEventKnown = map[string]bool{
	"seq": true, "task": true, "tag": true, "kind": true, "status": true,
	"owner": true, "revision": true, "count": true, "action": true, "at": true,
}

var claimKnownKeys = map[string]bool{
	"owner": true, "token": true, "ttl": true, "acquiredAt": true, "expiresAt": true,
}

var worktreeKnownKeys = map[string]bool{
	"path": true, "boundary": true, "branch": true, "createdAt": true, "keep": true,
}

type legacySubtaskSet struct {
	byID    map[string]*backlog.Subtask
	ordered []*backlog.Subtask
}

// validateLegacyGraph enforces the per-tag invariants a valid legacy store
// carries: unique task and per-parent subtask ids, nextId above every allocated
// task id, milestone references resolving against the tag registry, dependency
// references that exist (top-level or subtask), and no dependency cycles.
func validateLegacyGraph(snap *legacySnapshot) error {
	for _, name := range snap.tagName {
		entry := snap.tags[name]
		ids := map[string]bool{}
		subtasks := map[string]legacySubtaskSet{}
		maxID := 0
		for _, task := range entry.tasks {
			if ids[task.ID] {
				return legacyRefusalf("duplicate task id %s in tag %q", task.ID, name)
			}
			ids[task.ID] = true
			if n, err := strconv.Atoi(task.ID); err == nil && n > maxID {
				maxID = n
			}

			var rawSubtasks []json.RawMessage
			if err := json.Unmarshal(task.Subtasks, &rawSubtasks); err != nil {
				return legacyRefusalf("task %s subtasks in tag %q: %v", task.ID, name, err)
			}
			set := legacySubtaskSet{byID: map[string]*backlog.Subtask{}}
			for i, raw := range rawSubtasks {
				var fields struct {
					Dependencies []json.RawMessage `json:"dependencies"`
				}
				if err := json.Unmarshal(raw, &fields); err != nil {
					return legacyRefusalf("legacy tag %q task %s subtask %d: %v", name, task.ID, i, err)
				}
				for _, rawDependency := range fields.Dependencies {
					var dependency string
					if err := json.Unmarshal(rawDependency, &dependency); err == nil {
						continue
					}
					var number json.Number
					if err := json.Unmarshal(rawDependency, &number); err == nil && strings.Contains(number.String(), ".") {
						return legacyRefusalf("legacy tag %q task %s subtask %d: qualified dependency %s must be a string", name, task.ID, i, number)
					}
				}
				subtask, err := backlog.DecodeSubtask(task.ID, raw)
				if err != nil {
					return legacyRefusalf("legacy tag %q task %s subtask %d: %v", name, task.ID, i, err)
				}
				if set.byID[subtask.ID] != nil {
					return legacyRefusalf("duplicate subtask id %q under task %s in tag %q", subtask.ID, task.ID, name)
				}
				set.byID[subtask.ID] = subtask
				set.ordered = append(set.ordered, subtask)
				ids[task.ID+"."+subtask.ID] = true
			}
			subtasks[task.ID] = set
		}
		if int(entry.nextID) <= maxID {
			return legacyRefusalf("tag %q nextId %d is not above task id %d", name, entry.nextID, maxID)
		}
		milestoneNames := map[string]bool{}
		if len(entry.milestones) > 0 {
			registry, err := decodeJSONArray(entry.milestones)
			if err != nil {
				return legacyRefusalf("tag %q milestones registry: %v", name, err)
			}
			for _, e := range registry {
				obj, ok := e.(map[string]any)
				if !ok {
					return legacyRefusalf("tag %q milestones registry entry is not an object", name)
				}
				if n, ok := obj["name"].(string); ok {
					milestoneNames[n] = true
				}
			}
		}
		edges := map[string][]string{}
		for _, task := range entry.tasks {
			if task.Milestone != "" && !milestoneNames[task.Milestone] {
				return legacyRefusalf("task %s references unknown milestone %q in tag %q", task.ID, task.Milestone, name)
			}
			for _, dep := range task.Dependencies {
				parentID, subID, err := backlog.ParseTaskReference(dep)
				if err != nil {
					return legacyRefusalf("task %s dependency %q in tag %q: %v", task.ID, dep, name, err)
				}
				if _, ok := snap.lookupTask(name, parentID); !ok {
					return legacyRefusalf("task %s dependency %s does not exist in tag %q", task.ID, dep, name)
				}
				target := parentID
				if subID != "" {
					if subtasks[parentID].byID[subID] == nil {
						return legacyRefusalf("task %s dependency %s does not exist in tag %q", task.ID, dep, name)
					}
					target = parentID + "." + subID
				}
				edges[task.ID] = append(edges[task.ID], target)
			}
			for _, subtask := range subtasks[task.ID].ordered {
				source := task.ID + "." + subtask.ID
				seenDependencies := map[string]bool{}
				for _, dep := range subtask.Dependencies {
					parentID, subID, err := backlog.ParseTaskReference(dep)
					if err != nil {
						return legacyRefusalf("subtask %s dependency %q in tag %q: %v", source, dep, name, err)
					}
					targetParent := parentID
					targetSub := subID
					if subID == "" {
						targetParent = task.ID
						targetSub = parentID
					}
					target := targetParent + "." + targetSub
					if target == source {
						return legacyRefusalf("subtask %s depends on itself in tag %q", source, name)
					}
					if seenDependencies[target] {
						return legacyRefusalf("subtask %s has duplicate dependency %q in tag %q", source, dep, name)
					}
					seenDependencies[target] = true
					if subtasks[targetParent].byID[targetSub] == nil {
						return legacyRefusalf("subtask %s dependency %s does not exist in tag %q", source, dep, name)
					}
					edges[source] = append(edges[source], target)
				}
			}
		}
		if err := checkAcyclic(ids, edges); err != nil {
			return legacyRefusalf("tag %q: %v", name, err)
		}
	}
	return nil
}

func (s *legacySnapshot) lookupTask(tag, id string) (*Task, bool) {
	entry, ok := s.tags[tag]
	if !ok {
		return nil, false
	}
	for _, task := range entry.tasks {
		if task.ID == id {
			return task, true
		}
	}
	return nil, false
}

func checkAcyclic(ids map[string]bool, edges map[string][]string) error {
	const white, grey, black = 0, 1, 2
	color := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case grey:
			return fmt.Errorf("dependency cycle through task %s", id)
		case black:
			return nil
		}
		color[id] = grey
		for _, dep := range edges[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		color[id] = black
		return nil
	}
	for id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

// --- source-side digest and inventory ---

// legacyComparison computes the §8.5 comparison digest and shared inventory
// directly from the exact logical legacy content, using the same record
// shapes and within-class order as the imported payload. Legacy-only classes
// (assignments, receipts, plans, planMappings, runs, attempts, worktreeSagas)
// contribute no records.
func legacyComparison(snap *legacySnapshot) (string, *ClassInventory, error) {
	cd := newClassDigester()
	inv := &ClassInventory{}
	for _, class := range semanticClasses {
		if !class.comparison {
			continue
		}
		records, err := legacyClassRecords(snap, class.name)
		if err != nil {
			return "", nil, err
		}
		cd.emit(class.name, true, records)
		if err := inv.add(class.name, len(records)); err != nil {
			return "", nil, err
		}
	}
	_, comparison := cd.sums()
	return comparison, inv, nil
}

func legacyClassRecords(snap *legacySnapshot, class string) ([][]byte, error) {
	switch class {
	case "tags", "milestones":
		return legacyTagClassRecords(snap, class)
	case "tasks", "dependencies", "subtasks", "bindings", "claims":
		return legacyTaskClassRecords(snap, class)
	case "fence":
		rec, err := canonicalJSON(map[string]any{"counter": snap.fencing})
		if err != nil {
			return nil, err
		}
		return [][]byte{rec}, nil
	case "events":
		return legacyEventClassRecords(snap)
	default:
		return nil, fmt.Errorf("unknown comparison class %q", class)
	}
}

// legacyTagClassRecords mirrors tagClassRecords: tag name byte order, the
// stored counters, milestone registry entries by index.
func legacyTagClassRecords(snap *legacySnapshot, class string) ([][]byte, error) {
	var records [][]byte
	for _, name := range snap.tagName {
		entry := snap.tags[name]
		switch class {
		case "tags":
			record := map[string]any{
				"name":           name,
				"nextId":         entry.nextID,
				"taskCount":      entry.taskCount,
				"completedCount": entry.completedCount,
			}
			if err := mergeExtraRaw(record, entry.extra); err != nil {
				return nil, fmt.Errorf("tag %q: %w", name, err)
			}
			rec, err := canonicalJSON(record)
			if err != nil {
				return nil, err
			}
			records = append(records, rec)
		case "milestones":
			if len(entry.milestones) == 0 {
				continue
			}
			registry, err := decodeJSONArray(entry.milestones)
			if err != nil {
				return nil, fmt.Errorf("tag %q milestones registry: %w", name, err)
			}
			for i, entryValue := range registry {
				rec, err := canonicalJSON(map[string]any{"tag": name, "index": int64(i), "value": entryValue})
				if err != nil {
					return nil, err
				}
				records = append(records, rec)
			}
		}
	}
	return records, nil
}

// legacyTaskClassRecords mirrors taskClassRecords: (tag, numeric task id)
// order, then registry index. The record bodies come from the shared
// taskRecords renderer, so source and payload shapes cannot drift.
func legacyTaskClassRecords(snap *legacySnapshot, class string) ([][]byte, error) {
	tasks := make([]*Task, 0)
	for _, name := range snap.tagName {
		tasks = append(tasks, snap.tags[name].tasks...)
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].Tag != tasks[j].Tag {
			return tasks[i].Tag < tasks[j].Tag
		}
		a, aerr := strconv.Atoi(tasks[i].ID)
		b, berr := strconv.Atoi(tasks[j].ID)
		if aerr != nil || berr != nil {
			return tasks[i].ID < tasks[j].ID
		}
		return a < b
	})
	var records [][]byte
	for _, task := range tasks {
		recs, err := taskRecords(task, class)
		if err != nil {
			return nil, err
		}
		records = append(records, recs...)
	}
	return records, nil
}

// legacyEventClassRecords mirrors eventClassRecords: numeric sequence order,
// optional members only when present, unknown fields flattened.
func legacyEventClassRecords(snap *legacySnapshot) ([][]byte, error) {
	events := append([]Event(nil), snap.events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	var records [][]byte
	for _, e := range events {
		record := map[string]any{"seq": e.Seq, "kind": e.Kind, "createdAt": e.At}
		if e.Tag != "" {
			record["tag"] = e.Tag
		}
		if e.Task != "" {
			record["task"] = e.Task
		}
		if e.Status != "" {
			record["status"] = e.Status
		}
		if e.Owner != "" {
			record["owner"] = e.Owner
		}
		if e.Revision != 0 {
			record["revision"] = int64(e.Revision)
		}
		if e.Count != 0 {
			record["count"] = int64(e.Count)
		}
		if e.Action != "" {
			record["action"] = e.Action
		}
		if err := mergeExtraRaw(record, e.Extra); err != nil {
			return nil, fmt.Errorf("event %d: %w", e.Seq, err)
		}
		rec, err := canonicalJSON(record)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, nil
}

// --- import ---

// importLegacySnapshot loads the validated snapshot into the fresh temporary
// payload in one transaction, recording the deterministic family identity.
// No synthetic mutation events are appended: the feed is exactly the legacy
// retained event log, and every counter, id, revision, and timestamp is
// carried across unchanged.
func importLegacySnapshot(db *DB, snap *legacySnapshot, familyID, ownerBoundary string) error {
	// This is the private exact-v3 compatibility constructor, not a normal
	// authority mutation. Bypass immediate's current-schema upgrade so the
	// source comparison proof observes the unaugmented v3 import.
	return db.transact("BEGIN IMMEDIATE", func(t *tx) error {
		if _, err := t.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, metaFamilyID, familyID); err != nil {
			return fmt.Errorf("record legacy family id: %w", err)
		}
		if _, err := t.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, metaOwnerBoundary, ownerBoundary); err != nil {
			return fmt.Errorf("record legacy owner boundary: %w", err)
		}
		for _, name := range snap.tagName {
			entry := snap.tags[name]
			milestones := sql.NullString{}
			if len(entry.milestones) > 0 {
				milestones = sql.NullString{String: string(entry.milestones), Valid: true}
			}
			extra, err := encodeExtraChecked(entry.extra)
			if err != nil {
				return fmt.Errorf("import tag %q unknown fields: %w", name, err)
			}
			if _, err := t.Exec(`INSERT INTO tags(name, next_id, task_count, completed_count, milestones, extra) VALUES (?, ?, ?, ?, ?, ?)`,
				name, entry.nextID, entry.taskCount, entry.completedCount, milestones, extra); err != nil {
				return fmt.Errorf("import tag %q: %w", name, err)
			}
			for _, task := range entry.tasks {
				if err := validateTaskForWrite(task); err != nil {
					return fmt.Errorf("import tag %q: %w", name, err)
				}
				if err := t.insertTask(task); err != nil {
					return err
				}
			}
		}
		if _, err := t.Exec(`UPDATE meta SET value=? WHERE key='fencing'`, fmt.Sprint(snap.fencing)); err != nil {
			return fmt.Errorf("import fencing counter: %w", err)
		}
		for _, e := range snap.events {
			if err := t.appendEvent(e); err != nil {
				return err
			}
		}
		// Seed the events AUTOINCREMENT high-water mark so the next appended
		// event takes exactly the legacy nextSeq even when the retained log
		// was truncated below it (SPEC §8.5: restore and later mutations
		// never reuse or rewind a published sequence).
		seedSeq := snap.nextSeq - 1
		res, seedErr := t.Exec(`UPDATE sqlite_sequence SET seq=? WHERE name='events'`, seedSeq)
		if seedErr != nil {
			return fmt.Errorf("seed legacy event sequence: %w", seedErr)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := t.Exec(`INSERT INTO sqlite_sequence(name, seq) VALUES ('events', ?)`, seedSeq); err != nil {
				return fmt.Errorf("seed legacy event sequence: %w", err)
			}
		}
		var violations int
		if err := t.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
			return fmt.Errorf("foreign-key check: %w", err)
		}
		if violations > 0 {
			return fmt.Errorf("foreign-key check: %d violation(s)", violations)
		}
		var quick string
		if err := t.QueryRow(`PRAGMA quick_check`).Scan(&quick); err != nil {
			return fmt.Errorf("integrity check: %w", err)
		}
		if quick != "ok" {
			return fmt.Errorf("integrity check: %s", quick)
		}
		return nil
	})
}

// --- small parsing helpers ---

func rawObject(raw json.RawMessage, what string) (map[string]json.RawMessage, error) {
	if isJSONNullRaw(raw) {
		return nil, fmt.Errorf("%s is null", what)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("%s is not an object: %w", what, err)
	}
	if obj == nil {
		return nil, fmt.Errorf("%s is null", what)
	}
	return obj, nil
}

func requireKeys(obj map[string]json.RawMessage, what string, required []string, optional ...string) error {
	known := map[string]bool{}
	for _, key := range append(append([]string(nil), required...), optional...) {
		known[key] = true
	}
	for _, key := range required {
		if _, ok := obj[key]; !ok {
			return legacyRefusalf("%s is missing required key %q", what, key)
		}
	}
	for key := range obj {
		if !known[key] {
			return legacyRefusalf("%s has unknown key %q", what, key)
		}
	}
	return nil
}

func collectExtra(obj map[string]json.RawMessage, known map[string]bool, dst *map[string]json.RawMessage) error {
	for key, raw := range obj {
		if known[key] || len(raw) == 0 {
			continue
		}
		if *dst == nil {
			*dst = map[string]json.RawMessage{}
		}
		(*dst)[key] = raw
	}
	return nil
}

func decodeLegacyInt(raw json.RawMessage, dst *int64) error {
	if len(raw) == 0 || isJSONNullRaw(raw) {
		return errors.New("is null")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("is not an integer: %w", err)
	}
	return nil
}

func isJSONNullRaw(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return v == nil
}
