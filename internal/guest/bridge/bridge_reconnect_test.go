//go:build linux

package bridge

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/Microsoft/hcsshim/internal/guest/prot"
	"github.com/sirupsen/logrus"
)

func TestBridge_NotificationQueuedWhenDisconnected(t *testing.T) {
	b := New(nil, false)
	// Bridge starts disconnected (connected == false).
	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "c1"},
	})
	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "c2"},
	})

	b.notifyMu.Lock()
	if len(b.pendingNotifications) != 2 {
		t.Fatalf("expected 2 queued, got %d", len(b.pendingNotifications))
	}
	b.notifyMu.Unlock()
}

func TestBridge_DrainOnReconnect(t *testing.T) {
	b := New(nil, false)

	// Queue notifications while disconnected.
	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "c1"},
	})
	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "c2"},
	})

	// Simulate what ListenAndServe does: create channels, start writer,
	// then drain.
	b.responseChan = make(chan bridgeResponse, 4)

	b.drainPendingNotifications()

	// Collect drained notifications.
	var ids []string
	for i := 0; i < 2; i++ {
		select {
		case resp := <-b.responseChan:
			n := resp.response.(*prot.ContainerNotification)
			ids = append(ids, n.ContainerID)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for notification %d", i)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 drained notifications, got %d", len(ids))
	}

	b.notifyMu.Lock()
	if len(b.pendingNotifications) != 0 {
		t.Fatalf("expected 0 pending after drain, got %d", len(b.pendingNotifications))
	}
	b.notifyMu.Unlock()
}

func TestBridge_DisconnectQueuesAfterDrain(t *testing.T) {
	b := New(nil, false)
	b.responseChan = make(chan bridgeResponse, 4)

	// Drain with nothing pending — just sets connected = true.
	b.drainPendingNotifications()

	// Send while connected — goes directly to responseChan.
	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "live"},
	})

	select {
	case resp := <-b.responseChan:
		n := resp.response.(*prot.ContainerNotification)
		if n.ContainerID != "live" {
			t.Fatalf("expected 'live', got %q", n.ContainerID)
		}
	default:
		t.Fatal("expected notification on responseChan")
	}

	// Disconnect — future notifications should queue.
	b.disconnectNotifications()

	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "queued"},
	})

	b.notifyMu.Lock()
	if len(b.pendingNotifications) != 1 {
		t.Fatalf("expected 1 queued after disconnect, got %d", len(b.pendingNotifications))
	}
	b.notifyMu.Unlock()

	// Nothing should be on responseChan.
	select {
	case <-b.responseChan:
		t.Fatal("should not have received on responseChan after disconnect")
	default:
	}
}

func TestBridge_FullReconnectCycle(t *testing.T) {
	b := New(nil, false)

	// --- Iteration 1: simulate ListenAndServe ---
	r1, w1 := io.Pipe()
	b.responseChan = make(chan bridgeResponse, 4)
	b.quitChan = make(chan bool)

	go func() {
		for range b.responseChan {
		}
	}() // drain writer

	b.drainPendingNotifications()

	// Send a notification while connected.
	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "iter1"},
	})

	// Simulate bridge drop — disconnect, close channels.
	b.disconnectNotifications()
	close(b.quitChan)
	close(b.responseChan)
	r1.Close()
	w1.Close()

	// --- Between iterations: container exits ---
	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "between"},
	})

	b.notifyMu.Lock()
	if len(b.pendingNotifications) != 1 || b.pendingNotifications[0].ContainerID != "between" {
		t.Fatalf("expected 'between' queued, got %v", b.pendingNotifications)
	}
	b.notifyMu.Unlock()

	// --- Iteration 2: reconnect ---
	b.responseChan = make(chan bridgeResponse, 4)
	b.quitChan = make(chan bool)

	b.drainPendingNotifications()

	select {
	case resp := <-b.responseChan:
		n := resp.response.(*prot.ContainerNotification)
		if n.ContainerID != "between" {
			t.Fatalf("expected 'between', got %q", n.ContainerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for drained notification")
	}
}

// TestBridge_ListenAndServe_DisconnectReconnect simulates the live-migration
// bridge lifecycle end-to-end: serve over a real pipe, kill the pipe, then
// reuse the same Bridge struct over a brand-new pipe and verify it serves
// requests again. This is the in-process equivalent of an LM bridge drop +
// reconnect that the GCS reconnect loop in cmd/gcs/main.go performs.
func TestBridge_ListenAndServe_DisconnectReconnect(t *testing.T) {
	logrus.SetOutput(io.Discard)

	mux := NewBridgeMux()
	mux.HandleFunc(prot.ComputeSystemResizeConsoleV1, prot.PvInvalid,
		func(r *Request) (RequestResponse, error) {
			return &prot.MessageResponseBase{}, nil
		})

	b := New(mux, false)

	// --- Iteration 1 ---
	lc1 := newLoopbackConnection()
	serveDone1 := make(chan error, 1)
	go func() {
		serveDone1 <- b.ListenAndServe(lc1.SRead(), lc1.SWrite())
	}()

	if err := serverSend(lc1.CWrite(), prot.ComputeSystemResizeConsoleV1, prot.SequenceID(1),
		&prot.ContainerResizeConsole{
			MessageBase: prot.MessageBase{ContainerID: "c1", ActivityID: "a1"},
		}); err != nil {
		t.Fatalf("iter1 send: %v", err)
	}
	hdr, _, err := serverRead(lc1.CRead())
	if err != nil {
		t.Fatalf("iter1 read: %v", err)
	}
	if hdr.ID != prot.SequenceID(1) {
		t.Fatalf("iter1 wrong seq: got %d want 1", hdr.ID)
	}

	// Simulate bridge disconnect (host shim drops connection during LM).
	lc1.close()

	// ListenAndServe should return when the pipe is closed.
	select {
	case <-serveDone1:
	case <-time.After(2 * time.Second):
		t.Fatal("iter1 ListenAndServe did not return after pipe close")
	}

	// --- Iteration 2: same Bridge, brand-new pipe (LM reconnect path) ---
	lc2 := newLoopbackConnection()
	defer lc2.close()
	serveDone2 := make(chan error, 1)
	go func() {
		serveDone2 <- b.ListenAndServe(lc2.SRead(), lc2.SWrite())
	}()

	if err := serverSend(lc2.CWrite(), prot.ComputeSystemResizeConsoleV1, prot.SequenceID(2),
		&prot.ContainerResizeConsole{
			MessageBase: prot.MessageBase{ContainerID: "c1", ActivityID: "a2"},
		}); err != nil {
		t.Fatalf("iter2 send: %v", err)
	}
	hdr, _, err = serverRead(lc2.CRead())
	if err != nil {
		t.Fatalf("iter2 read: %v", err)
	}
	if hdr.ID != prot.SequenceID(2) {
		t.Fatalf("iter2 wrong seq: got %d want 2", hdr.ID)
	}

	// Cleanly tear down iter2: closing the loopback causes ListenAndServe
	// to see EOF on its read side and return.
	lc2.close()
	select {
	case <-serveDone2:
	case <-time.After(2 * time.Second):
		t.Fatal("iter2 ListenAndServe did not return after pipe close")
	}
}

// TestBridge_NotificationsSurviveReconnect verifies the queue-and-drain
// behavior across a real ListenAndServe disconnect/reconnect: notifications
// emitted while disconnected are buffered and delivered to the host on the
// next connection.
func TestBridge_NotificationsSurviveReconnect(t *testing.T) {
	logrus.SetOutput(io.Discard)

	b := New(NewBridgeMux(), false)

	// --- Iteration 1: connect, then immediately drop ---
	lc1 := newLoopbackConnection()
	serveDone1 := make(chan error, 1)
	go func() {
		serveDone1 <- b.ListenAndServe(lc1.SRead(), lc1.SWrite())
	}()

	// Wait for ListenAndServe to mark itself connected.
	deadline := time.Now().Add(time.Second)
	for {
		b.notifyMu.Lock()
		connected := b.connected
		b.notifyMu.Unlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge did not reach connected state")
		}
		time.Sleep(10 * time.Millisecond)
	}

	lc1.close()
	select {
	case <-serveDone1:
	case <-time.After(2 * time.Second):
		t.Fatal("iter1 did not return after disconnect")
	}

	// --- Between iterations: container exits queue notifications ---
	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "during-disconnect-1"},
	})
	b.publishNotification(&prot.ContainerNotification{
		MessageBase: prot.MessageBase{ContainerID: "during-disconnect-2"},
	})

	// --- Iteration 2: reconnect, expect the notifications to be drained ---
	lc2 := newLoopbackConnection()
	defer lc2.close()
	serveDone2 := make(chan error, 1)
	go func() {
		serveDone2 <- b.ListenAndServe(lc2.SRead(), lc2.SWrite())
	}()

	seen := make(map[string]bool)
	for i := 0; i < 2; i++ {
		hdr, body, err := serverRead(lc2.CRead())
		if err != nil {
			t.Fatalf("read drained notification %d: %v", i, err)
		}
		if hdr.Type != prot.ComputeSystemNotificationV1 {
			t.Fatalf("expected ComputeSystemNotificationV1, got %v", hdr.Type)
		}
		var n prot.ContainerNotification
		if err := json.Unmarshal(body, &n); err != nil {
			t.Fatalf("unmarshal notification %d: %v", i, err)
		}
		seen[n.ContainerID] = true
	}
	if !seen["during-disconnect-1"] || !seen["during-disconnect-2"] {
		t.Fatalf("expected both queued notifications to drain, got %v", seen)
	}

	// Cleanly tear down iter2: closing the loopback causes ListenAndServe
	// to see EOF on its read side and return.
	lc2.close()
	select {
	case <-serveDone2:
	case <-time.After(2 * time.Second):
		t.Fatal("iter2 ListenAndServe did not return after pipe close")
	}
}

