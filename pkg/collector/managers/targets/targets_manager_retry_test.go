package targets_manager

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/gnmic/pkg/api/types"
	collstore "github.com/openconfig/gnmic/pkg/collector/store"
	"google.golang.org/grpc"
)

// capsOnlyServer answers Capabilities, which is all a target's start needs.
type capsOnlyServer struct {
	gnmi.UnimplementedGNMIServer
}

func (capsOnlyServer) Capabilities(context.Context, *gnmi.CapabilityRequest) (*gnmi.CapabilityResponse, error) {
	return &gnmi.CapabilityResponse{GNMIVersion: "0.10.0"}, nil
}

// freeAddr reserves a local port and releases it, so the first dial is refused.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func serveGNMI(t *testing.T, addr string) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	gnmi.RegisterGNMIServer(s, capsOnlyServer{})
	go func() { _ = s.Serve(l) }()
	t.Cleanup(s.Stop)
}

func retryTestConfig(name, addr string) *types.TargetConfig {
	insecure := true
	return &types.TargetConfig{
		Name:       name,
		Address:    addr,
		Insecure:   &insecure,
		Timeout:    time.Second,
		RetryTimer: 100 * time.Millisecond,
	}
}

func waitForState(t *testing.T, tm *TargetsManager, name, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if tm.getTargetStateStr(name) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("target %s: state %q, want %q after %s", name, tm.getTargetStateStr(name), want, within)
}

func TestApply_retriesTargetWhoseFirstStartFailed(t *testing.T) {
	tm := newTargetsTestManager(t)
	addr := freeAddr(t)

	// Nothing listens yet: the first start fails, as against a switch still booting.
	tm.apply("t1", retryTestConfig("t1", addr))
	if st := tm.getTargetStateStr("t1"); st != collstore.StateFailed {
		t.Fatalf("first start: state %q, want %q", st, collstore.StateFailed)
	}

	// The device comes up. No new config is applied: the retry alone must start it.
	serveGNMI(t, addr)
	waitForState(t, tm, "t1", collstore.StateRunning, 5*time.Second)

	mt := tm.Lookup("t1")
	mt.RLock()
	pending := mt.retryCancel != nil
	mt.RUnlock()
	if pending {
		t.Fatal("retry still scheduled after the target started")
	}
}

func TestRemove_stopsRetry(t *testing.T) {
	tm := newTargetsTestManager(t)
	addr := freeAddr(t)

	tm.apply("t1", retryTestConfig("t1", addr))
	if st := tm.getTargetStateStr("t1"); st != collstore.StateFailed {
		t.Fatalf("first start: state %q, want %q", st, collstore.StateFailed)
	}
	tm.remove("t1")

	// A device appearing after removal must not resurrect the target.
	serveGNMI(t, addr)
	time.Sleep(500 * time.Millisecond)
	if tm.Lookup("t1") != nil {
		t.Fatal("removed target came back")
	}
	if st := tm.getTargetStateStr("t1"); st != "" {
		t.Fatalf("removed target has state %q", st)
	}
}

func TestApply_noRetryWhenStartSucceeds(t *testing.T) {
	tm := newTargetsTestManager(t)
	addr := freeAddr(t)
	serveGNMI(t, addr)

	tm.apply("t1", retryTestConfig("t1", addr))
	waitForState(t, tm, "t1", collstore.StateRunning, 5*time.Second)

	mt := tm.Lookup("t1")
	mt.RLock()
	pending := mt.retryCancel != nil
	mt.RUnlock()
	if pending {
		t.Fatal("a retry was scheduled for a target that started")
	}
}

func TestSetIntendedStateDisabled_stopsRetry(t *testing.T) {
	tm := newTargetsTestManager(t)
	addr := freeAddr(t)

	tm.apply("t1", retryTestConfig("t1", addr))
	if st := tm.getTargetStateStr("t1"); st != collstore.StateFailed {
		t.Fatalf("first start: state %q, want %q", st, collstore.StateFailed)
	}
	tm.SetIntendedState("t1", collstore.IntendedStateDisabled)

	// Checked before the retry's first tick (100ms), so only the cancel
	// itself can have cleared it.
	mt := tm.Lookup("t1")
	mt.RLock()
	pending := mt.retryCancel != nil
	mt.RUnlock()
	if pending {
		t.Fatal("retry still scheduled after the target was disabled")
	}

	// A disabled target must stay down when its device appears.
	serveGNMI(t, addr)
	time.Sleep(500 * time.Millisecond)
	if st := tm.getTargetStateStr("t1"); st == collstore.StateRunning {
		t.Fatalf("disabled target was started by a retry")
	}
}

func TestSetIntendedStateEnabled_retriesWhenStartFails(t *testing.T) {
	tm := newTargetsTestManager(t)
	addr := freeAddr(t)

	tm.apply("t1", retryTestConfig("t1", addr))
	tm.SetIntendedState("t1", collstore.IntendedStateDisabled)
	// Re-enabled while the device is still down: this start fails too.
	tm.SetIntendedState("t1", collstore.IntendedStateEnabled)

	serveGNMI(t, addr)
	waitForState(t, tm, "t1", collstore.StateRunning, 5*time.Second)
}
