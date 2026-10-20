package prot

import (
	"testing"

	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/protocol/guestresource"
)

func podMemoryLimitMessage(settings string) []byte {
	return []byte(`{"Request":{"ResourceType":"PodMemoryLimit","RequestType":"Update","Settings":` + settings + `}}`)
}

func TestUnmarshalContainerModifySettingsPodMemoryLimit(t *testing.T) {
	request, err := UnmarshalContainerModifySettings(podMemoryLimitMessage(`{"PodID":"pod1","LimitInBytes":134217728}`))
	if err != nil {
		t.Fatalf("UnmarshalContainerModifySettings returned %v", err)
	}
	msr := request.Request.(*guestrequest.ModificationRequest)
	s, ok := msr.Settings.(*guestresource.LCOWPodMemoryLimit)
	if !ok {
		t.Fatalf("Settings type %T, want *guestresource.LCOWPodMemoryLimit", msr.Settings)
	}
	if msr.ResourceType != guestresource.ResourceTypePodMemoryLimit || msr.RequestType != guestrequest.RequestTypeUpdate ||
		s.PodID != "pod1" || s.LimitInBytes == nil || *s.LimitInBytes != 134217728 {
		t.Fatalf("decoded %+v / %+v, want Update pod1 limit 134217728", msr, s)
	}
}

func TestUnmarshalMappedVirtioFS(t *testing.T) {
	raw := []byte(`{"Request":{"ResourceType":"MappedVirtioFS","RequestType":"Add","Settings":{"Tag":"hcsshim-shares","ShareName":"0","MountPath":"/run/mounts/plan9/0","ReadOnly":true}}}`)
	req, err := UnmarshalContainerModifySettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	msr := req.Request.(*guestrequest.ModificationRequest)
	got, ok := msr.Settings.(*guestresource.LCOWMappedVirtioFS)
	if !ok {
		t.Fatalf("Settings type %T, want *guestresource.LCOWMappedVirtioFS", msr.Settings)
	}
	if msr.ResourceType != guestresource.ResourceTypeMappedVirtioFS || got.ShareName != "0" || !got.ReadOnly || got.Tag != "hcsshim-shares" || got.MountPath != "/run/mounts/plan9/0" {
		t.Fatalf("got %+v / %+v", msr, got)
	}
}

func TestUnmarshalContainerModifySettingsPodMemoryLimitRejectsMalformed(t *testing.T) {
	for name, settings := range map[string]string{
		"unknown field":   `{"PodID":"pod1","LimitInBytes":134217728,"Linux":{"cpu":{"quota":1}}}`,
		"string limit":    `{"PodID":"pod1","LimitInBytes":"134217728"}`,
		"array settings":  `[]`,
		"string settings": `"pod1"`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := UnmarshalContainerModifySettings(podMemoryLimitMessage(settings)); err == nil {
				t.Fatalf("settings %s decoded without error", settings)
			}
		})
	}
}
