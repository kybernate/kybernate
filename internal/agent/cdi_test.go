package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kybernate/kybernate/internal/inventory"
)

const nvidiaSpec = `cdiVersion: 0.5.0
kind: nvidia.com/gpu
devices:
- name: "0"
- name: GPU-a
- name: all
containerEdits: {}
`

func writeSpec(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCDISpecsMissing(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "nvidia.yaml", nvidiaSpec)
	writeSpec(t, dir, "other.yaml", "cdiVersion: 0.5.0\nkind: vendor.com/net\ndevices:\n- name: GPU-b\n")
	writeSpec(t, dir, "notes.txt", "not a spec")
	c := CDISpecs{Dirs: []string{filepath.Join(dir, "missing"), dir}, Kind: "nvidia.com/gpu"}

	missing, err := c.Missing([]inventory.GPU{{UUID: "GPU-a"}, {UUID: "GPU-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, []string{"GPU-b"}) {
		t.Fatalf("missing = %v", missing)
	}
}

func TestCDISpecsWithoutSpecification(t *testing.T) {
	c := CDISpecs{Dirs: []string{t.TempDir()}, Kind: "nvidia.com/gpu"}
	if _, err := c.Missing([]inventory.GPU{{UUID: "GPU-a"}}); err == nil || !strings.Contains(err.Error(), "no CDI specification") {
		t.Fatalf("err = %v", err)
	}
}
