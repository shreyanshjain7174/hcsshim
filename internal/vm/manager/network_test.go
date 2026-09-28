//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/hcsshim/internal/hcs/resourcepaths"
	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	hcs "github.com/Microsoft/hcsshim/internal/hcs/v2"
	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/protocol/guestresource"
	"github.com/Microsoft/hcsshim/internal/vm/vmutils"
	"github.com/Microsoft/hcsshim/internal/vmservice"

	"github.com/Microsoft/go-winio/pkg/guid"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// fakeModifyVMClient implements vmservice.VMClient with a real ModifyResource and stub
// success everywhere else, so network Modify tests never need the full lifecycle fake.
type fakeModifyVMClient struct {
	mu            sync.Mutex
	totalCalls    int
	createCalls   []*vmservice.CreateVMRequest
	createErr     error
	modifyCalls   []*vmservice.ModifyResourceRequest
	modifyErrs    []error
	modifyEntered chan vmservice.ModifyType
	modifyRelease <-chan struct{}
	quitCalls     int
	quitBlock     <-chan struct{}
	teardownCalls int
	waitBlock     <-chan struct{}
	waitErr       error
	resumeEntered chan struct{}
	resumeBlock   <-chan struct{}
}

func (f *fakeModifyVMClient) ModifyResource(ctx context.Context, in *vmservice.ModifyResourceRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.totalCalls++
	f.modifyCalls = append(f.modifyCalls, in)
	entered := f.modifyEntered
	release := f.modifyRelease
	var err error
	if len(f.modifyErrs) == 0 {
		err = nil
	} else {
		err = f.modifyErrs[0]
		if len(f.modifyErrs) > 1 {
			f.modifyErrs = f.modifyErrs[1:]
		}
	}
	f.mu.Unlock()

	if entered != nil {
		entered <- in.GetType()
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &emptypb.Empty{}, err
}

func (f *fakeModifyVMClient) calls() []*vmservice.ModifyResourceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*vmservice.ModifyResourceRequest, len(f.modifyCalls))
	copy(out, f.modifyCalls)
	return out
}

func (f *fakeModifyVMClient) CreateVM(_ context.Context, in *vmservice.CreateVMRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.totalCalls++
	f.createCalls = append(f.createCalls, in)
	return &emptypb.Empty{}, f.createErr
}
func (f *fakeModifyVMClient) TeardownVM(ctx context.Context, _ *emptypb.Empty, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.totalCalls++
	f.teardownCalls++
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
func (f *fakeModifyVMClient) PauseVM(context.Context, *emptypb.Empty, ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.totalCalls++
	f.mu.Unlock()
	return &emptypb.Empty{}, nil
}

func (f *fakeModifyVMClient) ResumeVM(ctx context.Context, _ *emptypb.Empty, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.totalCalls++
	entered, block := f.resumeEntered, f.resumeBlock
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &emptypb.Empty{}, nil
}
func (f *fakeModifyVMClient) WaitVM(ctx context.Context, _ *emptypb.Empty, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.totalCalls++
	block, err := f.waitBlock, f.waitErr
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
func (f *fakeModifyVMClient) CapabilitiesVM(context.Context, *emptypb.Empty, ...grpc.CallOption) (*vmservice.CapabilitiesVMResponse, error) {
	f.mu.Lock()
	f.totalCalls++
	f.mu.Unlock()
	return &vmservice.CapabilitiesVMResponse{}, nil
}
func (f *fakeModifyVMClient) PropertiesVM(context.Context, *vmservice.PropertiesVMRequest, ...grpc.CallOption) (*vmservice.PropertiesVMResponse, error) {
	f.mu.Lock()
	f.totalCalls++
	f.mu.Unlock()
	return &vmservice.PropertiesVMResponse{}, nil
}
func (f *fakeModifyVMClient) AddPcieDevice(context.Context, *vmservice.AddPcieDeviceRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.totalCalls++
	f.mu.Unlock()
	return &emptypb.Empty{}, nil
}
func (f *fakeModifyVMClient) RemovePcieDevice(context.Context, *vmservice.RemovePcieDeviceRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.totalCalls++
	f.mu.Unlock()
	return &emptypb.Empty{}, nil
}
func (f *fakeModifyVMClient) Quit(ctx context.Context, _ *emptypb.Empty, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.totalCalls++
	f.quitCalls++
	block := f.quitBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (f *fakeModifyVMClient) quitCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quitCalls
}

func (f *fakeModifyVMClient) teardownCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.teardownCalls
}

func (f *fakeModifyVMClient) totalCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.totalCalls
}

var _ vmservice.VMClient = (*fakeModifyVMClient)(nil)

func TestFakeModifyVMClientCountsEveryRPC(t *testing.T) {
	client := &fakeModifyVMClient{}
	ctx := context.Background()
	calls := []struct {
		name string
		call func() error
	}{
		{name: "CreateVM", call: func() error { _, err := client.CreateVM(ctx, &vmservice.CreateVMRequest{}); return err }},
		{name: "TeardownVM", call: func() error { _, err := client.TeardownVM(ctx, &emptypb.Empty{}); return err }},
		{name: "PauseVM", call: func() error { _, err := client.PauseVM(ctx, &emptypb.Empty{}); return err }},
		{name: "ResumeVM", call: func() error { _, err := client.ResumeVM(ctx, &emptypb.Empty{}); return err }},
		{name: "WaitVM", call: func() error { _, err := client.WaitVM(ctx, &emptypb.Empty{}); return err }},
		{name: "CapabilitiesVM", call: func() error { _, err := client.CapabilitiesVM(ctx, &emptypb.Empty{}); return err }},
		{name: "PropertiesVM", call: func() error { _, err := client.PropertiesVM(ctx, &vmservice.PropertiesVMRequest{}); return err }},
		{name: "ModifyResource", call: func() error { _, err := client.ModifyResource(ctx, &vmservice.ModifyResourceRequest{}); return err }},
		{name: "AddPcieDevice", call: func() error { _, err := client.AddPcieDevice(ctx, &vmservice.AddPcieDeviceRequest{}); return err }},
		{name: "RemovePcieDevice", call: func() error { _, err := client.RemovePcieDevice(ctx, &vmservice.RemovePcieDeviceRequest{}); return err }},
		{name: "Quit", call: func() error { _, err := client.Quit(ctx, &emptypb.Empty{}); return err }},
	}

	for _, call := range calls {
		if err := call.call(); err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
	}
	if got := client.totalCallCount(); got != len(calls) {
		t.Fatalf("total VM RPC count = %d, want %d", got, len(calls))
	}
	if got := len(client.createCalls); got != 1 {
		t.Fatalf("CreateVM count = %d, want 1", got)
	}
	if got := len(client.calls()); got != 1 {
		t.Fatalf("ModifyResource count = %d, want 1", got)
	}
	if got := client.teardownCallCount(); got != 1 {
		t.Fatalf("TeardownVM count = %d, want 1", got)
	}
	if got := client.quitCallCount(); got != 1 {
		t.Fatalf("Quit count = %d, want 1", got)
	}
}

// fakeEndpointPortBinder is the test double for EndpointPortBinder. bindResults and
// unbindErrs are queues: each call pops the front entry, and the last entry repeats once
// the queue is drained to zero.
type fakeEndpointPortBinder struct {
	mu sync.Mutex

	bindCalls []struct {
		endpointID string
		nicID      guid.GUID
	}
	unbindCalls []struct {
		endpointID string
		portID     guid.GUID
		nicID      guid.GUID
	}

	bindResults []struct {
		bound BoundEndpoint
		err   error
	}
	unbindErrs    []error
	unbindEntered chan struct{}
	unbindBlock   <-chan struct{}

	// bindBlock parks Bind until it is closed, and deliberately ignores the caller's
	// context, so a test can hold an operation that refuses to cooperate with cancellation.
	bindEntered chan struct{}
	bindBlock   <-chan struct{}
}

func (f *fakeEndpointPortBinder) Bind(_ context.Context, endpointID string, nicID guid.GUID) (BoundEndpoint, error) {
	f.mu.Lock()
	f.bindCalls = append(f.bindCalls, struct {
		endpointID string
		nicID      guid.GUID
	}{endpointID, nicID})
	entered, block := f.bindEntered, f.bindBlock
	if entered != nil {
		f.bindEntered = nil
	}
	var r struct {
		bound BoundEndpoint
		err   error
	}
	if len(f.bindResults) > 0 {
		r = f.bindResults[0]
		if len(f.bindResults) > 1 {
			f.bindResults = f.bindResults[1:]
		}
	}
	f.mu.Unlock()

	if entered != nil {
		close(entered)
	}
	if block != nil {
		<-block
	}
	return r.bound, r.err
}

func (f *fakeEndpointPortBinder) Unbind(_ context.Context, endpointID string, portID guid.GUID, nicID guid.GUID) error {
	f.mu.Lock()
	f.unbindCalls = append(f.unbindCalls, struct {
		endpointID string
		portID     guid.GUID
		nicID      guid.GUID
	}{endpointID, portID, nicID})
	entered, block := f.unbindEntered, f.unbindBlock
	if entered != nil {
		close(entered)
		f.unbindEntered = nil
	}
	if len(f.unbindErrs) == 0 {
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		return nil
	}
	err := f.unbindErrs[0]
	if len(f.unbindErrs) > 1 {
		f.unbindErrs = f.unbindErrs[1:]
	}
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func (f *fakeEndpointPortBinder) bindCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bindCalls)
}

func (f *fakeEndpointPortBinder) unbindCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.unbindCalls)
}

func (f *fakeEndpointPortBinder) lastUnbind() (endpointID string, portID guid.GUID, nicID guid.GUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.unbindCalls[len(f.unbindCalls)-1]
	return last.endpointID, last.portID, last.nicID
}

func newTestSystem(client vmservice.VMClient, binder EndpointPortBinder) *System {
	return newSystem("test-system", guid.GUID{}, "", client, nil, nil, nil, binder)
}

func mustGUID(t *testing.T, s string) guid.GUID {
	t.Helper()
	g, err := guid.FromString(s)
	if err != nil {
		t.Fatalf("guid.FromString(%q): %v", s, err)
	}
	return g
}

const (
	testNicID      = "11111111-1111-1111-1111-111111111111"
	testEndpointID = "22222222-2222-2222-2222-222222222222"
	testSwitchID   = "33333333-3333-3333-3333-333333333333"
	testPortID     = "44444444-4444-4444-4444-444444444444"
	testMAC        = "12-34-56-78-9A-BC"
)

func validBoundEndpoint(t *testing.T) BoundEndpoint {
	return BoundEndpoint{
		PortID:     mustGUID(t, testPortID),
		EndpointID: testEndpointID,
		SwitchID:   testSwitchID,
		MacAddress: testMAC,
	}
}

func TestSystemModifySCSIUnchangedAndTouchesNoHCN(t *testing.T) {
	client := &fakeModifyVMClient{}
	binder := &fakeEndpointPortBinder{}
	sys := newTestSystem(client, binder)
	req := &hcsschema.ModifySettingRequest{
		RequestType:  guestrequest.RequestTypeAdd,
		ResourcePath: fmt.Sprintf(resourcepaths.SCSIResourceFormat, guestrequest.ScsiControllerGuids[0], 0),
		Settings: hcsschema.Attachment{
			Type_: "VirtualDisk",
			Path:  `C:\layer.vhdx`,
		},
	}

	if err := sys.Modify(context.Background(), req); err != nil {
		t.Fatalf("Modify(SCSI add): %v", err)
	}
	calls := client.calls()
	if len(calls) != 1 || calls[0].GetScsiDisk() == nil {
		t.Fatalf("calls = %+v, want one SCSI request", calls)
	}
	if binder.bindCallCount() != 0 || binder.unbindCallCount() != 0 {
		t.Fatalf("SCSI modify touched HCN: bind=%d unbind=%d", binder.bindCallCount(), binder.unbindCallCount())
	}
}

func TestSystemModifyPlan9ReturnsUnimplementedWithoutRPC(t *testing.T) {
	tests := []struct {
		name        string
		requestType guestrequest.RequestType
		settings    hcsschema.Plan9Share
		guest       guestresource.LCOWMappedDirectory
	}{
		{
			name:        "Add",
			requestType: guestrequest.RequestTypeAdd,
			settings: hcsschema.Plan9Share{
				Name:       "0",
				AccessName: "0",
				Path:       `C:\host-directory`,
				Port:       vmutils.Plan9Port,
				Flags:      hcsschema.Plan9ShareFlagsLinuxMetadata | hcsschema.Plan9ShareFlagsReadOnly,
			},
			guest: guestresource.LCOWMappedDirectory{
				MountPath: "/run/mounts/plan9/0",
				ShareName: "0",
				Port:      vmutils.Plan9Port,
				ReadOnly:  true,
			},
		},
		{
			name:        "Remove",
			requestType: guestrequest.RequestTypeRemove,
			settings: hcsschema.Plan9Share{
				Name:       "0",
				AccessName: "0",
				Port:       vmutils.Plan9Port,
			},
			guest: guestresource.LCOWMappedDirectory{
				MountPath: "/run/mounts/plan9/0",
				ShareName: "0",
				Port:      vmutils.Plan9Port,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeModifyVMClient{}
			binder := &fakeEndpointPortBinder{}
			sys := newTestSystem(client, binder)
			req := &hcsschema.ModifySettingRequest{
				RequestType:  test.requestType,
				ResourcePath: resourcepaths.Plan9ShareResourcePath,
				Settings:     test.settings,
				GuestRequest: guestrequest.ModificationRequest{
					ResourceType: guestresource.ResourceTypeMappedDirectory,
					RequestType:  test.requestType,
					Settings:     test.guest,
				},
			}

			err := sys.Modify(context.Background(), req)
			if !errors.Is(err, ErrModifyNotSupported) || !errors.Is(err, errdefs.ErrNotImplemented) {
				t.Fatalf("Modify(Plan9 %s) = %v, want ErrModifyNotSupported wrapping ErrNotImplemented", test.name, err)
			}
			if got := status.Code(errgrpc.ToGRPC(err)); got != codes.Unimplemented {
				t.Fatalf("Modify(Plan9 %s) gRPC code = %s, want %s", test.name, got, codes.Unimplemented)
			}
			if got := client.totalCallCount(); got != 0 {
				t.Fatalf("Modify(Plan9 %s) issued %d vmservice RPCs, want 0", test.name, got)
			}
			if binder.bindCallCount() != 0 || binder.unbindCallCount() != 0 {
				t.Fatalf("Plan9 %s touched HCN: bind=%d unbind=%d", test.name, binder.bindCallCount(), binder.unbindCallCount())
			}
		})
	}
}

func networkAddRequest(settings *hcsschema.NetworkAdapter) *hcsschema.ModifySettingRequest {
	return &hcsschema.ModifySettingRequest{
		RequestType:  guestrequest.RequestTypeAdd,
		ResourcePath: fmt.Sprintf(resourcepaths.NetworkResourceFormat, testNicID),
		Settings:     settings,
	}
}

func networkRemoveRequest(settings *hcsschema.NetworkAdapter) *hcsschema.ModifySettingRequest {
	return &hcsschema.ModifySettingRequest{
		RequestType:  guestrequest.RequestTypeRemove,
		ResourcePath: fmt.Sprintf(resourcepaths.NetworkResourceFormat, testNicID),
		Settings:     settings,
	}
}

func TestSystemCloseReleasesNetworkBindingsAndRetriesFailure(t *testing.T) {
	client := &fakeModifyVMClient{}
	binder := &fakeEndpointPortBinder{
		bindResults: []struct {
			bound BoundEndpoint
			err   error
		}{{bound: validBoundEndpoint(t)}},
		unbindErrs: []error{errors.New("unbind failed"), nil},
	}
	sys := newSystem("close-network", guid.GUID{}, t.TempDir()+`\absent.sock`, client, nil, nil, nil, binder)
	if err := sys.Modify(context.Background(), networkAddRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC})); err != nil {
		t.Fatalf("network add: %v", err)
	}

	if err := sys.CloseCtx(context.Background()); err == nil || !strings.Contains(err.Error(), "unbind failed") {
		t.Fatalf("first CloseCtx error = %v", err)
	}
	if sys.closed || binder.unbindCallCount() != 1 {
		t.Fatalf("closed=%v unbind calls=%d after failure", sys.closed, binder.unbindCallCount())
	}
	if err := sys.CloseCtx(context.Background()); err != nil {
		t.Fatalf("retry CloseCtx: %v", err)
	}
	if !sys.closed || binder.unbindCallCount() != 2 {
		t.Fatalf("closed=%v unbind calls=%d after retry", sys.closed, binder.unbindCallCount())
	}
}

func TestSystemModifyNetworkAddFailureRollsBack(t *testing.T) {
	for _, test := range []struct {
		name        string
		bound       BoundEndpoint
		bindErr     error
		modifyErr   error
		modifyCalls int
		unbindCalls int
	}{
		{name: "bind failure before port allocation", bindErr: errors.New("bind failed")},
		{name: "partial bind failure", bound: BoundEndpoint{PortID: mustGUID(t, testPortID)}, bindErr: errors.New("bind failed"), unbindCalls: 1},
		{name: "post-bind mismatch", bound: BoundEndpoint{PortID: mustGUID(t, testPortID), EndpointID: testEndpointID, SwitchID: testSwitchID, MacAddress: "AA:BB:CC:DD:EE:FF"}, unbindCalls: 1},
		{name: "vmservice failure", bound: validBoundEndpoint(t), modifyErr: errors.New("vmservice add failed"), modifyCalls: 1, unbindCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeModifyVMClient{}
			if test.modifyErr != nil {
				client.modifyErrs = []error{test.modifyErr}
			}
			binder := &fakeEndpointPortBinder{
				bindResults: []struct {
					bound BoundEndpoint
					err   error
				}{{bound: test.bound, err: test.bindErr}},
			}
			sys := newTestSystem(client, binder)

			if err := sys.Modify(context.Background(), networkAddRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC})); err == nil {
				t.Fatal("Modify = nil, want add failure")
			}
			if len(client.calls()) != test.modifyCalls || binder.unbindCallCount() != test.unbindCalls {
				t.Fatalf("rollback calls: modify=%d want=%d unbind=%d want=%d", len(client.calls()), test.modifyCalls, binder.unbindCallCount(), test.unbindCalls)
			}
			if test.unbindCalls != 0 {
				endpointID, portID, nicID := binder.lastUnbind()
				if endpointID != testEndpointID || portID != mustGUID(t, testPortID) || nicID != mustGUID(t, testNicID) {
					t.Fatalf("unbind(%q, %v, %v), want exact attempted tuple", endpointID, portID, nicID)
				}
			}
			if _, exists := sys.lookupNetworkBinding(mustGUID(t, testNicID).String()); exists {
				t.Fatal("binding retained after successful rollback")
			}
		})
	}
}

func TestSystemModifyNetworkAddDuplicateRejectedWithoutExtraCalls(t *testing.T) {
	client := &fakeModifyVMClient{}
	binder := &fakeEndpointPortBinder{
		bindResults: []struct {
			bound BoundEndpoint
			err   error
		}{{bound: validBoundEndpoint(t)}},
	}
	sys := newTestSystem(client, binder)

	req := networkAddRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC})
	if err := sys.Modify(context.Background(), req); err != nil {
		t.Fatalf("first Modify: %v", err)
	}
	if err := sys.Modify(context.Background(), req); err == nil || !errors.Is(err, errDuplicateNetworkAdapter) {
		t.Fatalf("second Modify = %v, want errDuplicateNetworkAdapter", err)
	}
	if binder.bindCallCount() != 1 || len(client.calls()) != 1 {
		t.Fatalf("duplicate add issued extra calls: bind=%d modify=%d", binder.bindCallCount(), len(client.calls()))
	}
}

func TestSystemModifyNetworkAddSuccessRequestFields(t *testing.T) {
	client := &fakeModifyVMClient{}
	binder := &fakeEndpointPortBinder{
		bindResults: []struct {
			bound BoundEndpoint
			err   error
		}{{bound: BoundEndpoint{
			PortID:     mustGUID(t, testPortID),
			EndpointID: testEndpointID,
			SwitchID:   testSwitchID,
			MacAddress: "12:34:56:78:9a:bc", // different separator/case than the request
		}}},
	}
	sys := newTestSystem(client, binder)

	req := networkAddRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC})
	if err := sys.Modify(context.Background(), req); err != nil {
		t.Fatalf("Modify: %v", err)
	}

	calls := client.calls()
	if len(calls) != 1 {
		t.Fatalf("modify calls = %d, want 1", len(calls))
	}
	call := calls[0]
	nic := call.GetNicConfig()
	dio := nic.GetDio()
	if call.GetType() != vmservice.ModifyType_ADD || nic == nil || dio == nil {
		t.Fatalf("request = %+v, want ADD NicConfig with DIO backend", call)
	}
	if nic.GetNicId() != mustGUID(t, testNicID).String() || nic.GetMacAddress() != "12:34:56:78:9a:bc" || dio.GetSwitchId() != mustGUID(t, testSwitchID).String() || dio.GetPortId() != mustGUID(t, testPortID).String() {
		t.Fatalf("ADD tuple = %+v, want committed NIC/MAC/switch/port identity", nic)
	}
}

func addBoundNIC(t *testing.T, sys *System, binder *fakeEndpointPortBinder) {
	t.Helper()
	binder.mu.Lock()
	binder.bindResults = []struct {
		bound BoundEndpoint
		err   error
	}{{bound: validBoundEndpoint(t)}}
	binder.mu.Unlock()
	req := networkAddRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC})
	if err := sys.Modify(context.Background(), req); err != nil {
		t.Fatalf("setup add: %v", err)
	}
}

func TestSystemModifyNetworkRemoveRPCFailureRetainsBinding(t *testing.T) {
	client := &fakeModifyVMClient{}
	binder := &fakeEndpointPortBinder{}
	sys := newTestSystem(client, binder)
	addBoundNIC(t, sys, binder)

	client.mu.Lock()
	client.modifyErrs = []error{errors.New("vmservice remove failed")}
	client.mu.Unlock()

	req := networkRemoveRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC})
	if err := sys.Modify(context.Background(), req); err == nil {
		t.Fatalf("Modify = nil, want the vmservice error")
	}
	if binder.unbindCallCount() != 0 {
		t.Fatalf("unbind was called despite a vmservice remove failure")
	}
	if _, exists := sys.lookupNetworkBinding(mustGUID(t, testNicID).String()); !exists {
		t.Fatalf("binding was dropped despite a vmservice remove failure")
	}
}

func TestSystemModifyNetworkRemoveUnbindFailureRetainsThenRetrySucceeds(t *testing.T) {
	client := &fakeModifyVMClient{}
	binder := &fakeEndpointPortBinder{unbindErrs: []error{errors.New("hns unbind failed")}}
	sys := newTestSystem(client, binder)
	addBoundNIC(t, sys, binder)

	req := networkRemoveRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC})
	if err := sys.Modify(context.Background(), req); err == nil {
		t.Fatalf("first remove = nil, want the unbind error")
	}
	if _, exists := sys.lookupNetworkBinding(mustGUID(t, testNicID).String()); !exists {
		t.Fatalf("binding was dropped despite an unbind failure")
	}

	binder.mu.Lock()
	binder.unbindErrs = nil
	binder.mu.Unlock()

	if err := sys.Modify(context.Background(), req); err != nil {
		t.Fatalf("retry remove: %v", err)
	}
	if _, exists := sys.lookupNetworkBinding(mustGUID(t, testNicID).String()); exists {
		t.Fatalf("binding still present after a successful retry")
	}
	if binder.unbindCallCount() != 2 {
		t.Fatalf("unbind calls = %d, want 2 (failed then retried)", binder.unbindCallCount())
	}
	if len(client.calls()) != 2 {
		t.Fatalf("modify calls = %d, want one ADD and one REMOVE across the retry", len(client.calls()))
	}
	remove := client.calls()[1].GetNicConfig()
	if remove.GetNicId() != mustGUID(t, testNicID).String() || remove.GetMacAddress() != testMAC ||
		remove.GetDio().GetSwitchId() != mustGUID(t, testSwitchID).String() || remove.GetDio().GetPortId() != mustGUID(t, testPortID).String() {
		t.Fatalf("REMOVE tuple = %+v, want committed NIC/MAC/switch/port identity", remove)
	}
}

func TestSystemModifyNetworkRemoveSerializesConcurrentRequests(t *testing.T) {
	client := &fakeModifyVMClient{}
	binder := &fakeEndpointPortBinder{}
	sys := newTestSystem(client, binder)
	addBoundNIC(t, sys, binder)

	entered := make(chan vmservice.ModifyType, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	client.mu.Lock()
	client.modifyEntered = entered
	client.modifyRelease = release
	client.mu.Unlock()

	req := networkRemoveRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC})
	errs := make(chan error, 2)
	go func() { errs <- sys.Modify(context.Background(), req) }()
	if got := <-entered; got != vmservice.ModifyType_REMOVE {
		t.Fatalf("first concurrent call type = %v, want REMOVE", got)
	}

	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		errs <- sys.Modify(context.Background(), req)
	}()
	<-secondStarted

	select {
	case <-entered:
		t.Fatal("second REMOVE reached vmservice while the first REMOVE was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	releaseAll()
	firstErr := <-errs
	secondErr := <-errs
	if firstErr != nil && secondErr != nil {
		t.Fatalf("both concurrent removes failed: first=%v second=%v", firstErr, secondErr)
	}
	if !errors.Is(firstErr, errUnknownNetworkAdapter) && !errors.Is(secondErr, errUnknownNetworkAdapter) {
		t.Fatalf("one concurrent remove must observe the completed removal: first=%v second=%v", firstErr, secondErr)
	}
	if len(client.calls()) != 2 {
		t.Fatalf("modify calls = %d, want one ADD and one REMOVE", len(client.calls()))
	}
}

// TestSystemModifyNetworkAddRollbackFailureRetainsBindingForRelease covers the window
// where an add has a real HNS port but no committed NIC: if the rollback unbind also
// fails, the port must stay tracked so the close-time release worker can retry the exact
// tuple rather than leaking it.
func TestSystemModifyNetworkAddRollbackFailureRetainsBindingForRelease(t *testing.T) {
	primaryErr := errors.New("vmservice add failed")
	rollbackErr := errors.New("hns unbind failed")
	client := &fakeModifyVMClient{modifyErrs: []error{primaryErr}}
	binder := &fakeEndpointPortBinder{
		bindResults: []struct {
			bound BoundEndpoint
			err   error
		}{{bound: validBoundEndpoint(t)}},
		unbindErrs: []error{rollbackErr, nil},
	}
	sys := newSystem("rollback-retain", guid.GUID{}, filepath.Join(t.TempDir(), "absent.sock"), client, nil, nil, nil, binder)

	modifyErr := sys.Modify(context.Background(), networkAddRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC}))
	if !errors.Is(modifyErr, primaryErr) || !errors.Is(modifyErr, rollbackErr) {
		t.Fatalf("Modify error = %v, want primary and rollback failures", modifyErr)
	}
	key := mustGUID(t, testNicID).String()
	binding, exists := sys.lookupNetworkBinding(key)
	if !exists || binding.endpointID != testEndpointID || binding.portID != mustGUID(t, testPortID) {
		t.Fatalf("retained binding = %+v (exists=%v), want exact attempted endpoint/port", binding, exists)
	}

	if err := sys.CloseCtx(context.Background()); err != nil {
		t.Fatalf("CloseCtx: %v", err)
	}
	if binder.unbindCallCount() != 2 {
		t.Fatalf("unbind calls = %d, want rollback attempt plus release retry", binder.unbindCallCount())
	}
	if _, exists := sys.lookupNetworkBinding(key); exists {
		t.Fatal("binding survived successful release retry")
	}
}

// TestSystemModifyNetworkAddRejectedWhenClosingAfterTxnLock covers a network add that was
// admitted before CloseCtx set closing and then waited on networkTxnMu behind the release
// worker. It must not reach HNS at all once it finally holds the lock.
func TestSystemModifyNetworkAddRejectedWhenClosingAfterTxnLock(t *testing.T) {
	client := &fakeModifyVMClient{}
	binder := &fakeEndpointPortBinder{
		bindResults: []struct {
			bound BoundEndpoint
			err   error
		}{{bound: validBoundEndpoint(t)}},
	}
	sys := newTestSystem(client, binder)

	// Hold the transaction lock so the add cannot proceed, mark the system closing, and
	// only then release it: the add is admitted before closing and delayed behind it.
	sys.networkTxnMu.Lock()
	added := make(chan error, 1)
	go func() {
		added <- sys.modifyNetwork(context.Background(), networkAddRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC}))
	}()
	sys.lifecycleMu.Lock()
	sys.closing = true
	sys.lifecycleMu.Unlock()
	sys.networkTxnMu.Unlock()

	select {
	case err := <-added:
		if !errors.Is(err, hcs.ErrAlreadyClosed) {
			t.Fatalf("late network add = %v, want hcs.ErrAlreadyClosed", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("late network add never returned")
	}
	if binder.bindCallCount() != 0 {
		t.Fatalf("bind calls = %d, want 0: a closing system must not claim an HNS port", binder.bindCallCount())
	}
	if len(client.calls()) != 0 {
		t.Fatalf("vmservice calls = %d, want 0", len(client.calls()))
	}
	if _, exists := sys.lookupNetworkBinding(mustGUID(t, testNicID).String()); exists {
		t.Fatal("a rejected late add left a binding behind")
	}
}
