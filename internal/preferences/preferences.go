// Package preferences owns the closed, project-local generation preferences
// document used by project setup. It never stores credentials or executable
// paths.
package preferences

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	SchemaVersion    = 1
	MaxDocumentBytes = 16 * 1024
	MaxModelBytes    = 256

	AdapterAnthropicAPI = "anthropic-api"
	AdapterClaudeCLI    = "claude-cli"
	AdapterOMP          = "omp"
	AdapterCodex        = "codex"
	AdapterKimi         = "kimi"

	stateDirectory = ".tasks"
	fileName       = "preferences.json"
)

var (
	// ErrAdapterUnavailable means the installation-selected CLI executable
	// cannot be resolved as a regular executable.
	ErrAdapterUnavailable = errors.New("generation adapter executable is unavailable")
	// ErrAdapterContained means PATH selected an executable inside the project,
	// which project data is not allowed to control.
	ErrAdapterContained = errors.New("generation adapter executable is inside the project boundary")
)

// Generation is the complete generation-selection surface. Executables,
// arguments, environment, endpoints, and credentials are deliberately absent.
type Generation struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
}

// Preferences is the versioned project preferences document.
type Preferences struct {
	SchemaVersion int        `json:"schemaVersion"`
	Generation    Generation `json:"generation"`
}

// Snapshot preserves the exact bytes observed before setup takes the project
// lock. Exists distinguishes an absent document from an empty, invalid one.
type Snapshot struct {
	Exists      bool
	Raw         []byte
	Preferences Preferences
}

// Path returns the fixed preferences location for a project boundary.
func Path(boundary string) string {
	return filepath.Join(boundary, stateDirectory, fileName)
}

// Load safely reads and validates the fixed preferences document. An absent
// document is a successful snapshot with Exists false.
func Load(boundary string) (Snapshot, error) {
	if err := validateBoundary(boundary); err != nil {
		return Snapshot{}, err
	}
	tasksPath := filepath.Join(boundary, stateDirectory)
	exists, err := validateStateDirectory(tasksPath)
	if err != nil {
		return Snapshot{}, err
	}
	if !exists {
		return Snapshot{}, nil
	}

	raw, err := readRegularNoFollow(Path(boundary), MaxDocumentBytes+1)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read preferences: %w", err)
	}
	if len(raw) > MaxDocumentBytes {
		return Snapshot{}, fmt.Errorf("preferences document exceeds %d bytes", MaxDocumentBytes)
	}
	prefs, err := parse(raw)
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid preferences: %w", err)
	}
	return Snapshot{Exists: true, Raw: bytes.Clone(raw), Preferences: prefs}, nil
}

// Validate applies the complete value-level preferences contract.
func Validate(p Preferences) error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d", p.SchemaVersion)
	}
	if !validAdapter(p.Generation.Adapter) {
		return fmt.Errorf("unknown generation adapter %q", p.Generation.Adapter)
	}
	return validateModel(p.Generation.Model)
}

// MarshalCanonical returns the stable, reviewable representation written by
// setup. The document always ends in one newline.
func MarshalCanonical(p Preferences) ([]byte, error) {
	if err := Validate(p); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// Publish durably creates or replaces preferences at the fixed boundary path.
// New and replaced files always have mode 0644, independent of umask or prior
// mode.
func Publish(boundary string, p Preferences) error {
	if err := validateBoundary(boundary); err != nil {
		return err
	}
	raw, err := MarshalCanonical(p)
	if err != nil {
		return err
	}
	tasksPath := filepath.Join(boundary, stateDirectory)
	exists, err := validateStateDirectory(tasksPath)
	if err != nil {
		return err
	}
	if !exists {
		if err := os.Mkdir(tasksPath, 0o755); err != nil {
			return fmt.Errorf("create state directory: %w", err)
		}
		if err := syncDirectory(boundary); err != nil {
			return err
		}
	}
	if err := validatePublishTarget(Path(boundary)); err != nil {
		return err
	}
	return publishAtomic(Path(boundary), raw)
}

// AdapterAvailable checks only installation availability. It never executes a
// provider or probes authentication.
func AdapterAvailable(boundary, adapter string) (bool, error) {
	if !validAdapter(adapter) {
		return false, fmt.Errorf("unknown generation adapter %q", adapter)
	}
	if adapter == AdapterAnthropicAPI {
		return true, nil
	}
	_, err := ResolveAdapterExecutable(boundary, adapter)
	if errors.Is(err, ErrAdapterUnavailable) || errors.Is(err, ErrAdapterContained) {
		return false, nil
	}
	return err == nil, err
}

// ResolveAdapterExecutable snapshots the canonical executable selected by the
// fixed basename for a CLI adapter. Project data cannot choose the basename or
// redirect it back inside the project boundary.
func ResolveAdapterExecutable(boundary, adapter string) (string, error) {
	if !validAdapter(adapter) {
		return "", fmt.Errorf("unknown generation adapter %q", adapter)
	}
	if adapter == AdapterAnthropicAPI {
		return "", errors.New("anthropic-api is an in-process adapter")
	}
	if err := validateBoundary(boundary); err != nil {
		return "", err
	}
	name := adapter
	if adapter == AdapterClaudeCLI {
		name = "claude"
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", adapter, ErrAdapterUnavailable)
	}
	canonical, err := ValidateCLIExecutable(boundary, resolved)
	if err != nil {
		return "", fmt.Errorf("%s: %w", adapter, err)
	}
	return canonical, nil
}

// ValidateCLIExecutable canonicalizes one already-selected CLI executable and
// requires it to be a regular executable outside the project boundary. It is
// shared with the bounded legacy selector so both paths enforce one trust
// check; configured selection still chooses only fixed basenames above.
func ValidateCLIExecutable(boundary, resolved string) (string, error) {
	if err := validateBoundary(boundary); err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", ErrAdapterUnavailable
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", ErrAdapterUnavailable
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", ErrAdapterUnavailable
	}
	canonicalBoundary, err := filepath.EvalSymlinks(boundary)
	if err != nil {
		return "", fmt.Errorf("resolve project boundary: %w", err)
	}
	contained, err := pathContained(canonicalBoundary, canonical)
	if err != nil {
		return "", err
	}
	if contained {
		return "", ErrAdapterContained
	}
	return canonical, nil
}

func parse(raw []byte) (Preferences, error) {
	if !utf8.Valid(raw) {
		return Preferences{}, errors.New("document is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := expectDelimiter(dec, '{'); err != nil {
		return Preferences{}, err
	}

	var p Preferences
	seen := make(map[string]bool, 2)
	for dec.More() {
		key, err := objectKey(dec, seen)
		if err != nil {
			return Preferences{}, err
		}
		switch key {
		case "schemaVersion":
			version, err := literalVersion(dec)
			if err != nil {
				return Preferences{}, err
			}
			p.SchemaVersion = version
		case "generation":
			generation, err := decodeGeneration(dec)
			if err != nil {
				return Preferences{}, err
			}
			p.Generation = generation
		default:
			return Preferences{}, fmt.Errorf("unknown key %q", key)
		}
	}
	if err := expectDelimiter(dec, '}'); err != nil {
		return Preferences{}, err
	}
	if !seen["schemaVersion"] || !seen["generation"] {
		return Preferences{}, errors.New("schemaVersion and generation are required")
	}
	if token, err := dec.Token(); err != io.EOF {
		if err != nil {
			return Preferences{}, fmt.Errorf("trailing JSON: %w", err)
		}
		return Preferences{}, fmt.Errorf("trailing JSON value %v", token)
	}
	if err := Validate(p); err != nil {
		return Preferences{}, err
	}
	return p, nil
}

func decodeGeneration(dec *json.Decoder) (Generation, error) {
	if err := expectDelimiter(dec, '{'); err != nil {
		return Generation{}, fmt.Errorf("generation: %w", err)
	}
	var generation Generation
	seen := make(map[string]bool, 2)
	for dec.More() {
		key, err := objectKey(dec, seen)
		if err != nil {
			return Generation{}, fmt.Errorf("generation: %w", err)
		}
		value, err := stringValue(dec)
		if err != nil {
			return Generation{}, fmt.Errorf("generation.%s: %w", key, err)
		}
		switch key {
		case "adapter":
			generation.Adapter = value
		case "model":
			generation.Model = value
		default:
			return Generation{}, fmt.Errorf("generation: unknown key %q", key)
		}
	}
	if err := expectDelimiter(dec, '}'); err != nil {
		return Generation{}, fmt.Errorf("generation: %w", err)
	}
	if !seen["adapter"] || !seen["model"] {
		return Generation{}, errors.New("generation.adapter and generation.model are required")
	}
	return generation, nil
}

func objectKey(dec *json.Decoder, seen map[string]bool) (string, error) {
	token, err := dec.Token()
	if err != nil {
		return "", err
	}
	key, ok := token.(string)
	if !ok {
		return "", errors.New("object key is not a string")
	}
	if seen[key] {
		return "", fmt.Errorf("duplicate key %q", key)
	}
	seen[key] = true
	return key, nil
}

func expectDelimiter(dec *json.Decoder, want json.Delim) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	got, ok := token.(json.Delim)
	if !ok || got != want {
		return fmt.Errorf("expected %q", want)
	}
	return nil
}

func literalVersion(dec *json.Decoder) (int, error) {
	token, err := dec.Token()
	if err != nil {
		return 0, err
	}
	number, ok := token.(json.Number)
	if !ok || number.String() != "1" {
		return 0, errors.New("schemaVersion must be the integer 1")
	}
	return SchemaVersion, nil
}

func stringValue(dec *json.Decoder) (string, error) {
	token, err := dec.Token()
	if err != nil {
		return "", err
	}
	value, ok := token.(string)
	if !ok {
		return "", errors.New("must be a string")
	}
	return value, nil
}

func validateModel(model string) error {
	if strings.TrimSpace(model) != model || model == "" {
		return errors.New("generation model must be nonempty and trimmed")
	}
	if len(model) > MaxModelBytes {
		return fmt.Errorf("generation model exceeds %d bytes", MaxModelBytes)
	}
	if strings.HasPrefix(model, "-") {
		return errors.New("generation model must not begin with '-'")
	}
	for _, b := range []byte(model) {
		if b < 0x20 || b == 0x7f {
			return errors.New("generation model contains an ASCII control character")
		}
	}
	return nil
}

func validAdapter(adapter string) bool {
	switch adapter {
	case AdapterAnthropicAPI, AdapterClaudeCLI, AdapterOMP, AdapterCodex, AdapterKimi:
		return true
	default:
		return false
	}
}

func validateBoundary(boundary string) error {
	info, err := os.Stat(boundary)
	if err != nil {
		return fmt.Errorf("inspect project boundary: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("project boundary %s is not a directory", boundary)
	}
	return nil
}

func validateStateDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect state directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("state directory %s is a symlink", path)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("state path %s is not a directory", path)
	}
	return true, nil
}

func validatePublishTarget(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect preferences path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("preferences path %s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("preferences path %s is not a regular file", path)
	}
	return nil
}

func publishAtomic(path string, raw []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".noise-tasks-preferences-*.tmp")
	if err != nil {
		return fmt.Errorf("create preferences temp file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	defer temp.Close()

	if _, err := temp.Write(raw); err != nil {
		return fmt.Errorf("write preferences temp file: %w", err)
	}
	if err := temp.Chmod(0o644); err != nil {
		return fmt.Errorf("set preferences mode: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("fsync preferences: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close preferences temp file: %w", err)
	}
	if err := validatePublishTarget(path); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish preferences: %w", err)
	}
	return syncDirectory(dir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for fsync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fsync directory: %w", err)
	}
	return nil
}

func pathContained(boundary, candidate string) (bool, error) {
	relative, err := filepath.Rel(boundary, candidate)
	if err != nil {
		return false, fmt.Errorf("compare executable to project boundary: %w", err)
	}
	if relative == "." {
		return true, nil
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative), nil
}
