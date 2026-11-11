package autotrader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mgh3326/factortrail/internal/providers/autotrader/fake"
)

const repoRoot = "../../.."

type lockedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type lockedContract struct {
	Name               string       `json:"name"`
	SourceRepo         string       `json:"source_repo"`
	SourceCommit       string       `json:"source_commit"`
	SourcePath         string       `json:"source_path"`
	Path               string       `json:"path"`
	SHA256             string       `json:"sha256"`
	ExamplesSourcePath string       `json:"examples_source_path"`
	Examples           []lockedFile `json:"examples"`
}

func loadLock(t *testing.T) lockedContract {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "contracts.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Version   int              `json:"version"`
		Contracts []lockedContract `json:"contracts"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	if lock.Version != 1 || len(lock.Contracts) != 1 {
		t.Fatalf("contracts.lock.json: version %d with %d contracts, want version 1 with one", lock.Version, len(lock.Contracts))
	}
	return lock.Contracts[0]
}

func fileSHA(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const updateHint = "; to update the pin, follow \"Updating the pinned contract\" in docs/service.md"

// The pinned spec and examples must be byte-identical to what the lock
// records. Editing one byte of either fails here.
func TestPinnedContractMatchesLock(t *testing.T) {
	c := loadLock(t)
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(c.SourceCommit) {
		t.Errorf("source_commit %q is not a full commit SHA", c.SourceCommit)
	}
	if c.SourceRepo == "" || c.SourcePath == "" {
		t.Error("source_repo and source_path must be recorded")
	}
	if got := fileSHA(t, c.Path); got != c.SHA256 {
		t.Errorf("%s sha256 = %s, lock records %s%s", c.Path, got, c.SHA256, updateHint)
	}
	locked := map[string]bool{}
	for _, f := range c.Examples {
		locked[f.Path] = true
		if got := fileSHA(t, f.Path); got != f.SHA256 {
			t.Errorf("%s sha256 = %s, lock records %s%s", f.Path, got, f.SHA256, updateHint)
		}
	}
	// A file added to or removed from the examples directory is a change too.
	onDisk, err := filepath.Glob(filepath.Join(repoRoot, filepath.Dir(c.Path), "examples", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range onDisk {
		rel, _ := filepath.Rel(repoRoot, p)
		rel = filepath.ToSlash(rel)
		if !locked[rel] {
			t.Errorf("%s is not in contracts.lock.json%s", rel, updateHint)
		}
		delete(locked, rel)
	}
	for p := range locked {
		t.Errorf("%s is in contracts.lock.json but missing on disk%s", p, updateHint)
	}
}

// The pinned spec defines the nine operations, GET only, authenticated by
// the one header the provider sends.
func TestPinnedContractShape(t *testing.T) {
	c := loadLock(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot, c.Path))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
		} `yaml:"paths"`
		Components struct {
			SecuritySchemes map[string]struct {
				Type string `yaml:"type"`
				In   string `yaml:"in"`
				Name string `yaml:"name"`
			} `yaml:"securitySchemes"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	var ops []string
	for path, methods := range spec.Paths {
		if len(methods) != 1 || methods["get"].OperationID == "" {
			t.Errorf("%s: want exactly one GET operation, got %v", path, methods)
			continue
		}
		id := methods["get"].OperationID
		if want := "/trading/api/retro/v1/" + id; path != want {
			t.Errorf("operation %s is at %s, want %s", id, path, want)
		}
		ops = append(ops, id)
	}
	want := append([]string(nil), fake.Operations...)
	sort.Strings(want)
	sort.Strings(ops)
	if strings.Join(ops, ",") != strings.Join(want, ",") {
		t.Errorf("operations = %v, want %v", ops, want)
	}
	s := spec.Components.SecuritySchemes["RetroReadToken"]
	if s.Type != "apiKey" || s.In != "header" || s.Name != TokenHeader {
		t.Errorf("RetroReadToken = %+v, want apiKey in header %s", s, TokenHeader)
	}
}
