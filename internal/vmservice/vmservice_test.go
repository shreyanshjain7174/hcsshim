// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package vmservice

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestVirtioFSConfigReadOnlyDescriptorAndRoundTrip(t *testing.T) {
	message := (&VirtioFSConfig{}).ProtoReflect()
	field := message.Descriptor().Fields().ByName("read_only")
	if field == nil {
		t.Fatal("VirtioFSConfig.read_only descriptor is missing")
	}
	if got, want := field.Number(), protoreflect.FieldNumber(3); got != want {
		t.Fatalf("VirtioFSConfig.read_only field number = %d, want %d", got, want)
	}
	if got, want := field.Kind(), protoreflect.BoolKind; got != want {
		t.Fatalf("VirtioFSConfig.read_only kind = %v, want %v", got, want)
	}
	if field.HasPresence() {
		t.Fatal("VirtioFSConfig.read_only has explicit presence, want implicit presence")
	}
	if message.Get(field).Bool() {
		t.Fatal("VirtioFSConfig.read_only default = true, want false")
	}

	wire, err := proto.Marshal(&VirtioFSConfig{})
	if err != nil {
		t.Fatalf("marshal default VirtioFSConfig: %v", err)
	}
	if len(wire) != 0 {
		t.Fatalf("default VirtioFSConfig wire bytes = %x, want no fields", wire)
	}

	wire, err = proto.Marshal(&VirtioFSConfig{ReadOnly: true})
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
	decodedField := decoded.ProtoReflect().Descriptor().Fields().ByNumber(3)
	if decodedField == nil || decodedField.Name() != "read_only" {
		t.Fatalf("VirtioFSConfig field 3 descriptor = %v, want read_only", decodedField)
	}
	if !decoded.GetReadOnly() || !decoded.ProtoReflect().Get(decodedField).Bool() {
		t.Fatal("VirtioFSConfig.read_only = false after round trip, want true")
	}
}
