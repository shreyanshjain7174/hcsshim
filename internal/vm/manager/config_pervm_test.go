//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestConfigForVMDerivesDistinctPaths(t *testing.T) {
	c := &Config{
		OpenVMMBinaryPath: `C:\cp\openvmm.exe`,
		VMServiceSocket:   `C:\cp\ovm\vm.sock`,
		HybridVsockBase:   `C:\cp\ovm\hv`,
		SerialSocket:      `C:\cp\ovm\com1.sock`,
	}
	a, b := c.ForVM("pod-a@vm"), c.ForVM("pod-b@vm")
	if a.VMServiceSocket == b.VMServiceSocket || a.HybridVsockBase == b.HybridVsockBase || a.SerialSocket == b.SerialSocket {
		t.Fatalf("paths collide: %+v %+v", a, b)
	}
	if *c.ForVM("pod-a@vm") != *a {
		t.Fatal("derivation is not deterministic")
	}
	if !regexp.MustCompile(`\\vm-[0-9a-f]{8}\.sock$`).MatchString(a.VMServiceSocket) ||
		!regexp.MustCompile(`\\hv-[0-9a-f]{8}$`).MatchString(a.HybridVsockBase) ||
		!regexp.MustCompile(`\\com1-[0-9a-f]{8}\.sock$`).MatchString(a.SerialSocket) {
		t.Fatalf("unexpected shape: %+v", a)
	}
	if a.OpenVMMBinaryPath != c.OpenVMMBinaryPath || c.VMServiceSocket != `C:\cp\ovm\vm.sock` {
		t.Fatal("ForVM must copy, not mutate")
	}
}

func TestValidateReservesPerVMSuffix(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "openvmm.exe")
	if err := os.WriteFile(binary, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{
		OpenVMMBinaryPath: binary,
		VMServiceSocket:   `C:\vm.sock`,
		HybridVsockBase:   `C:\hv`,
		SerialSocket:      `C:\com1.sock`,
	}
	if _, err := c.Validate(); err != nil {
		t.Fatalf("short paths rejected: %v", err)
	}
	// Fits the raw AF_UNIX limit but not once the per-VM suffix is added.
	c.VMServiceSocket = `C:\` + strings.Repeat("a", afUnixPathLimit-len(`C:\`)-perVMSuffixBytes+1)
	if _, err := c.Validate(); !errors.Is(err, errConfigSource) {
		t.Fatalf("over-budget socket template accepted: %v", err)
	}
	c.VMServiceSocket = `C:\vm.sock`
	c.HybridVsockBase = `C:\` + strings.Repeat("h", hybridVsockBaseLimit-len(`C:\`)-perVMSuffixBytes+1)
	if _, err := c.Validate(); !errors.Is(err, errConfigSource) {
		t.Fatalf("over-budget hybrid base template accepted: %v", err)
	}
}
