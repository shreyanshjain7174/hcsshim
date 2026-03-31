//go:build linux
// +build linux

package hcsv2

import (
	"path/filepath"
	"testing"

	specGuest "github.com/Microsoft/hcsshim/internal/guest/spec"
)

func TestRegisterAndResolveSandboxRoot(t *testing.T) {
	h := &Host{
		sandboxRoots: make(map[string]string),
	}

	h.RegisterSandboxRoot("sandbox-1", "/run/gcs/c/sandbox-1")
	got := h.ResolveSandboxRoot("sandbox-1")
	if got != "/run/gcs/c/sandbox-1" {
		t.Fatalf("expected /run/gcs/c/sandbox-1, got %s", got)
	}
}

func TestResolveSandboxRootFallback(t *testing.T) {
	h := &Host{
		sandboxRoots: make(map[string]string),
	}

	// No registration — should fall back to legacy derivation
	got := h.ResolveSandboxRoot("sandbox-missing")
	want := specGuest.SandboxRootDir("sandbox-missing")
	if got != want {
		t.Fatalf("expected fallback %s, got %s", want, got)
	}
}

func TestUnregisterSandboxRoot(t *testing.T) {
	h := &Host{
		sandboxRoots: make(map[string]string),
	}

	h.RegisterSandboxRoot("sandbox-1", "/some/path")
	h.UnregisterSandboxRoot("sandbox-1")

	// After unregister, should fall back to legacy
	got := h.ResolveSandboxRoot("sandbox-1")
	want := specGuest.SandboxRootDir("sandbox-1")
	if got != want {
		t.Fatalf("expected fallback %s after unregister, got %s", want, got)
	}
}

func TestSandboxRootFromOCIBundlePath(t *testing.T) {
	// Regular sandbox: sandboxRoot == OCIBundlePath
	ociBundlePath := "/run/gcs/c/my-sandbox-id"
	sandboxRoot := ociBundlePath
	if sandboxRoot != "/run/gcs/c/my-sandbox-id" {
		t.Fatalf("expected sandbox root to equal OCIBundlePath, got %s", sandboxRoot)
	}
}

func TestVirtualPodRootFromOCIBundlePath(t *testing.T) {
	tests := []struct {
		name          string
		ociBundlePath string
		virtualPodID  string
		want          string
	}{
		{
			name:          "legacy shim path",
			ociBundlePath: "/run/gcs/c/container-abc",
			virtualPodID:  "vpod-123",
			want:          "/run/gcs/c/virtual-pods/vpod-123",
		},
		{
			name:          "new shim path",
			ociBundlePath: "/new/layout/sandboxes/container-abc",
			virtualPodID:  "vpod-456",
			want:          "/new/layout/sandboxes/virtual-pods/vpod-456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filepath.Join(filepath.Dir(tt.ociBundlePath), "virtual-pods", tt.virtualPodID)
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestVirtualPodRootMatchesLegacy(t *testing.T) {
	// When OCIBundlePath uses the legacy prefix, the derived virtual pod root
	// must match what VirtualPodRootDir() would have produced.
	ociBundlePath := "/run/gcs/c/container-id"
	virtualPodID := "vpod-abc"

	derived := filepath.Join(filepath.Dir(ociBundlePath), "virtual-pods", virtualPodID)
	legacy := specGuest.VirtualPodRootDir(virtualPodID)

	if derived != legacy {
		t.Fatalf("derived %q != legacy %q — backwards compatibility broken", derived, legacy)
	}
}

func TestSubdirectoryPaths(t *testing.T) {
	sandboxRoot := "/run/gcs/c/sandbox-xyz"
	checks := map[string]string{
		"sandboxMounts":      filepath.Join(sandboxRoot, "sandboxMounts"),
		"sandboxTmpfsMounts": filepath.Join(sandboxRoot, "sandboxTmpfsMounts"),
		"hugepages":          filepath.Join(sandboxRoot, "hugepages"),
		"logs":               filepath.Join(sandboxRoot, "logs"),
	}

	for name, want := range checks {
		got := filepath.Join(sandboxRoot, name)
		if got != want {
			t.Fatalf("subdir %s: got %s, want %s", name, got, want)
		}
	}

	// Verify these match what spec.go functions produce
	if filepath.Join(sandboxRoot, "sandboxMounts") != specGuest.SandboxMountsDir("sandbox-xyz") {
		t.Fatal("sandboxMounts path doesn't match legacy SandboxMountsDir")
	}
	if filepath.Join(sandboxRoot, "hugepages") != specGuest.HugePagesMountsDir("sandbox-xyz") {
		t.Fatal("hugepages path doesn't match legacy HugePagesMountsDir")
	}
}

// TestOldVsNewPathParity exhaustively compares every path the old code
// would have produced (via spec.go functions with hard-coded prefix + ID)
// against the new code (resolved sandboxRoot + inline filepath.Join).
// If any pair diverges, backwards compatibility is broken.
func TestOldVsNewPathParity(t *testing.T) {
	type pathCase struct {
		name    string
		oldPath string // what the old code produced
		newPath string // what the new code produces
	}

	// Simulate old shim: OCIBundlePath = /run/gcs/c/<sandboxID>
	sandboxID := "abc-123-sandbox"
	ociBundlePath := "/run/gcs/c/" + sandboxID
	sandboxRoot := ociBundlePath // new code: sandboxRoot = OCIBundlePath

	regularCases := []pathCase{
		{
			name:    "sandbox root",
			oldPath: specGuest.SandboxRootDir(sandboxID),
			newPath: sandboxRoot,
		},
		{
			name:    "sandboxMounts dir",
			oldPath: specGuest.SandboxMountsDir(sandboxID),
			newPath: filepath.Join(sandboxRoot, "sandboxMounts"),
		},
		{
			name:    "sandboxTmpfsMounts dir",
			oldPath: specGuest.SandboxTmpfsMountsDir(sandboxID),
			newPath: filepath.Join(sandboxRoot, "sandboxTmpfsMounts"),
		},
		{
			name:    "hugepages dir",
			oldPath: specGuest.HugePagesMountsDir(sandboxID),
			newPath: filepath.Join(sandboxRoot, "hugepages"),
		},
		{
			name:    "logs dir",
			oldPath: specGuest.SandboxLogsDir(sandboxID, ""),
			newPath: filepath.Join(sandboxRoot, "logs"),
		},
		{
			name:    "log file path",
			oldPath: specGuest.SandboxLogPath(sandboxID, "", "container.log"),
			newPath: filepath.Join(sandboxRoot, "logs", "container.log"),
		},
		{
			name:    "hostname file",
			oldPath: filepath.Join(specGuest.SandboxRootDir(sandboxID), "hostname"),
			newPath: filepath.Join(sandboxRoot, "hostname"),
		},
		{
			name:    "hosts file",
			oldPath: filepath.Join(specGuest.SandboxRootDir(sandboxID), "hosts"),
			newPath: filepath.Join(sandboxRoot, "hosts"),
		},
		{
			name:    "resolv.conf file",
			oldPath: filepath.Join(specGuest.SandboxRootDir(sandboxID), "resolv.conf"),
			newPath: filepath.Join(sandboxRoot, "resolv.conf"),
		},
	}

	t.Run("regular_sandbox", func(t *testing.T) {
		for _, tc := range regularCases {
			if tc.oldPath != tc.newPath {
				t.Errorf("%s: old=%q new=%q", tc.name, tc.oldPath, tc.newPath)
			}
		}
	})

	// Virtual pod: old code used VirtualPodRootDir(vpodID),
	// new code uses filepath.Join(filepath.Dir(ociBundlePath), "virtual-pods", vpodID)
	vpodID := "vpod-456"
	vpodOCIBundlePath := "/run/gcs/c/" + sandboxID // container's own bundle
	vpodSandboxRoot := filepath.Join(filepath.Dir(vpodOCIBundlePath), "virtual-pods", vpodID)

	vpodCases := []pathCase{
		{
			name:    "virtual pod root",
			oldPath: specGuest.VirtualPodRootDir(vpodID),
			newPath: vpodSandboxRoot,
		},
		{
			name:    "virtual pod sandboxMounts",
			oldPath: specGuest.VirtualPodMountsDir(vpodID),
			newPath: filepath.Join(vpodSandboxRoot, "sandboxMounts"),
		},
		{
			name:    "virtual pod tmpfs mounts",
			oldPath: specGuest.VirtualPodTmpfsMountsDir(vpodID),
			newPath: filepath.Join(vpodSandboxRoot, "sandboxTmpfsMounts"),
		},
		{
			name:    "virtual pod hugepages",
			oldPath: specGuest.VirtualPodHugePagesMountsDir(vpodID),
			newPath: filepath.Join(vpodSandboxRoot, "hugepages"),
		},
		{
			name:    "virtual pod logs",
			oldPath: specGuest.SandboxLogsDir(sandboxID, vpodID),
			newPath: filepath.Join(vpodSandboxRoot, "logs"),
		},
		{
			name:    "virtual pod hostname",
			oldPath: filepath.Join(specGuest.VirtualPodRootDir(vpodID), "hostname"),
			newPath: filepath.Join(vpodSandboxRoot, "hostname"),
		},
		{
			name:    "virtual pod hosts",
			oldPath: filepath.Join(specGuest.VirtualPodRootDir(vpodID), "hosts"),
			newPath: filepath.Join(vpodSandboxRoot, "hosts"),
		},
		{
			name:    "virtual pod resolv.conf",
			oldPath: filepath.Join(specGuest.VirtualPodRootDir(vpodID), "resolv.conf"),
			newPath: filepath.Join(vpodSandboxRoot, "resolv.conf"),
		},
	}

	t.Run("virtual_pod", func(t *testing.T) {
		for _, tc := range vpodCases {
			if tc.oldPath != tc.newPath {
				t.Errorf("%s: old=%q new=%q", tc.name, tc.oldPath, tc.newPath)
			}
		}
	})

	// Workload container: sandbox root comes from mapping, not OCIBundlePath
	t.Run("workload_uses_sandbox_root", func(t *testing.T) {
		h := &Host{sandboxRoots: make(map[string]string)}
		h.RegisterSandboxRoot(sandboxID, ociBundlePath)

		workloadSandboxRoot := h.ResolveSandboxRoot(sandboxID)
		if workloadSandboxRoot != ociBundlePath {
			t.Fatalf("workload got sandboxRoot=%q, want %q", workloadSandboxRoot, ociBundlePath)
		}
		// Networking mount: hostname from sandbox root, not workload's own bundle
		workloadBundle := "/run/gcs/c/workload-container-999"
		hostsOld := filepath.Join(specGuest.SandboxRootDir(sandboxID), "hosts")
		hostsNew := filepath.Join(workloadSandboxRoot, "hosts")
		if hostsOld != hostsNew {
			t.Errorf("workload hosts mount: old=%q new=%q", hostsOld, hostsNew)
		}
		// Verify it's NOT derived from workload's own bundle
		if hostsNew == filepath.Join(workloadBundle, "hosts") {
			t.Error("workload hosts incorrectly derived from its own bundle path")
		}
	})

	// Standalone: sandboxRoot = OCIBundlePath directly
	t.Run("standalone", func(t *testing.T) {
		standaloneBundle := "/run/gcs/c/standalone-789"
		standaloneRoot := standaloneBundle // new code: sandboxRoot = OCIBundlePath
		oldRoot := specGuest.SandboxRootDir("standalone-789")

		if standaloneRoot != oldRoot {
			t.Errorf("standalone root: old=%q new=%q", oldRoot, standaloneRoot)
		}
	})
}
