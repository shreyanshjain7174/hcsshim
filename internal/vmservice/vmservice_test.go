// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package vmservice

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
)

// OpenVMM reads read_only from wire field 3; a mismatch makes read-only mounts writable.
func TestVirtioFSConfigReadOnlyWireEncoding(t *testing.T) {
	wire, err := proto.Marshal(&VirtioFSConfig{ReadOnly: true})
	if err != nil {
		t.Fatalf("marshal read-only VirtioFSConfig: %v", err)
	}
	if want := []byte{0x18, 0x01}; !bytes.Equal(wire, want) {
		t.Fatalf("read-only VirtioFSConfig wire bytes = %x, want %x", wire, want)
	}

	var decoded VirtioFSConfig
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal VirtioFSConfig: %v", err)
	}
	if !decoded.GetReadOnly() {
		t.Fatal("VirtioFSConfig.read_only = false after round trip, want true")
	}

	wire, err = proto.Marshal(&VirtioFSConfig{})
	if err != nil {
		t.Fatalf("marshal default VirtioFSConfig: %v", err)
	}
	if len(wire) != 0 {
		t.Fatalf("default VirtioFSConfig wire bytes = %x, want no fields", wire)
	}
}
