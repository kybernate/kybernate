package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/kybernate/kybernate/internal/inventory"
)

// DefaultCDISpecDirs are the directories containerd reads CDI specifications
// from. The NVIDIA Container Toolkit writes its specification to /var/run/cdi.
var DefaultCDISpecDirs = []string{"/var/run/cdi", "/etc/cdi"}

// CDISpecs checks that the CDI specifications on the node describe every GPU,
// so that the device plugin can hand GPUs to containers as CDI devices.
type CDISpecs struct {
	Dirs []string
	// Kind is the CDI device kind of the GPUs, for example nvidia.com/gpu.
	Kind string
}

// Missing returns the UUIDs of the GPUs that no CDI device of the kind names.
func (c CDISpecs) Missing(gpus []inventory.GPU) ([]string, error) {
	names, err := c.devices()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, gpu := range gpus {
		if !names[gpu.UUID] {
			missing = append(missing, gpu.UUID)
		}
	}
	return missing, nil
}

func (c CDISpecs) devices() (map[string]bool, error) {
	names := map[string]bool{}
	found := false
	for _, dir := range c.Dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", dir, err)
		}
		for _, e := range entries {
			ext := filepath.Ext(e.Name())
			if e.IsDir() || (ext != ".yaml" && ext != ".yml" && ext != ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			var spec struct {
				Kind    string `json:"kind"`
				Devices []struct {
					Name string `json:"name"`
				} `json:"devices"`
			}
			if err := yaml.Unmarshal(raw, &spec); err != nil {
				return nil, fmt.Errorf("parsing CDI specification %s: %w", e.Name(), err)
			}
			if spec.Kind != c.Kind {
				continue
			}
			found = true
			for _, d := range spec.Devices {
				names[d.Name] = true
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("no CDI specification of kind %s in %s", c.Kind, strings.Join(c.Dirs, ", "))
	}
	return names, nil
}
