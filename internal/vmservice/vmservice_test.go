package vmservice

import (
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
	if message.Get(field).Bool() {
		t.Fatal("VirtioFSConfig.read_only default = true, want false")
	}

	message.Set(field, protoreflect.ValueOfBool(true))
	wire, err := proto.Marshal(message.Interface())
	if err != nil {
		t.Fatalf("marshal VirtioFSConfig: %v", err)
	}

	var decoded VirtioFSConfig
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal VirtioFSConfig: %v", err)
	}
	decodedField := decoded.ProtoReflect().Descriptor().Fields().ByNumber(3)
	if decodedField == nil || decodedField.Name() != "read_only" {
		t.Fatalf("VirtioFSConfig field 3 descriptor = %v, want read_only", decodedField)
	}
	if !decoded.ProtoReflect().Get(decodedField).Bool() {
		t.Fatal("VirtioFSConfig.read_only = false after round trip, want true")
	}
}
