//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const configFileName = "openvmm-backend.json"

const (
	afUnixPathLimit            = 107
	hybridVsockWidestSuffix    = "_4294967295"
	hybridVsockBaseLimit       = afUnixPathLimit - len(hybridVsockWidestSuffix)
	hybridVsockGUIDSuffixBytes = 1 + 36
	hybridVsockBaseWarn        = afUnixPathLimit - hybridVsockGUIDSuffixBytes
)

var errConfigSource = errors.New("the openvmm backend configuration source is unusable")

// Config identifies the OpenVMM process and its host-side socket paths.
type Config struct {
	OpenVMMBinaryPath string `json:"openvmmBinaryPath"`
	VMServiceSocket   string `json:"vmServiceSocket"`
	HybridVsockBase   string `json:"hybridVsockBase"`
	SerialSocket      string `json:"serialSocket"`
}

var executableDir = func() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(self), nil
}

// LoadConfig reads openvmm-backend.json beside the running shim.
func LoadConfig() (*Config, error) {
	dir, err := executableDir()
	if err != nil {
		return nil, fmt.Errorf("cannot locate %s beside the running shim: %w (%v)", configFileName, errConfigSource, err)
	}
	path := filepath.Join(dir, configFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the openvmm backend configuration source %s: %w (%v)", path, errConfigSource, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	config := &Config{}
	if err := decoder.Decode(config); err != nil {
		return nil, fmt.Errorf("cannot decode the openvmm backend configuration source %s: %w (%v)", path, errConfigSource, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("the openvmm backend configuration source %s must hold exactly one JSON object and nothing after it: %w", path, errConfigSource)
	}
	if missing := config.missingFields(); len(missing) != 0 {
		return nil, fmt.Errorf("the openvmm backend configuration source %s is missing required field(s) %s: %w", path, strings.Join(missing, ", "), errConfigSource)
	}
	return config, nil
}

func (c *Config) missingFields() []string {
	var missing []string
	if c.OpenVMMBinaryPath == "" {
		missing = append(missing, "openvmmBinaryPath")
	}
	if c.VMServiceSocket == "" {
		missing = append(missing, "vmServiceSocket")
	}
	if c.HybridVsockBase == "" {
		missing = append(missing, "hybridVsockBase")
	}
	if c.SerialSocket == "" {
		missing = append(missing, "serialSocket")
	}
	return missing
}

// Validate checks paths and returns non-fatal compatibility warnings separately.
func (c *Config) Validate() (warnings []string, err error) {
	for _, field := range []struct{ name, value string }{
		{"openvmmBinaryPath", c.OpenVMMBinaryPath},
		{"vmServiceSocket", c.VMServiceSocket},
		{"hybridVsockBase", c.HybridVsockBase},
		{"serialSocket", c.SerialSocket},
	} {
		if !filepath.IsAbs(field.value) {
			return warnings, fmt.Errorf("%s in %s must be an absolute path, got %q: %w", field.name, configFileName, field.value, errConfigSource)
		}
	}
	if _, statErr := os.Stat(c.OpenVMMBinaryPath); statErr != nil {
		return warnings, fmt.Errorf("openvmmBinaryPath %q in %s does not exist: %w (%v)", c.OpenVMMBinaryPath, configFileName, errConfigSource, statErr)
	}
	if strings.Contains(c.VMServiceSocket, ",") {
		return warnings, fmt.Errorf("vmServiceSocket in %s contains a reserved comma in OpenVMM's --rpc value grammar: %w", configFileName, errConfigSource)
	}
	if strings.Contains(c.VMServiceSocket, "=") {
		return warnings, fmt.Errorf("vmServiceSocket in %s contains a reserved equals sign in OpenVMM's --rpc value grammar: %w", configFileName, errConfigSource)
	}
	for _, field := range []struct{ name, value string }{
		{"vmServiceSocket", c.VMServiceSocket},
		{"serialSocket", c.SerialSocket},
	} {
		if n := len(field.value); n > afUnixPathLimit {
			return warnings, fmt.Errorf("%s in %s is %d UTF-8 bytes, over the AF_UNIX limit of %d: %w", field.name, configFileName, n, afUnixPathLimit, errConfigSource)
		}
	}
	if n := len(c.HybridVsockBase); n > hybridVsockBaseLimit {
		return warnings, fmt.Errorf("hybridVsockBase in %s is %d UTF-8 bytes, over the limit of %d, which is the AF_UNIX limit of %d less the %d bytes of the widest %q suffix: %w", configFileName, n, hybridVsockBaseLimit, afUnixPathLimit, len(hybridVsockWidestSuffix), hybridVsockWidestSuffix, errConfigSource)
	}
	if n := len(c.HybridVsockBase); n > hybridVsockBaseWarn {
		warnings = append(warnings, fmt.Sprintf("hybridVsockBase is %d UTF-8 bytes; above %d bytes OpenVMM's %d-character GUID fallback suffix no longer fits inside the AF_UNIX limit of %d", n, hybridVsockBaseWarn, hybridVsockGUIDSuffixBytes-1, afUnixPathLimit))
	}
	return warnings, nil
}
