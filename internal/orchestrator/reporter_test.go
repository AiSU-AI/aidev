package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/agents"
)

// recordingReporter captures every event the orchestrator fires at it,
// so tests can assert the fan-out without reaching into private state.
type recordingReporter struct {
	events []Event
	fail   error
}

func (r *recordingReporter) OnEvent(_ context.Context, ev Event) error {
	r.events = append(r.events, ev)
	return r.fail
}
func (r *recordingReporter) Close() error { return nil }

func TestNullReporterIsZeroCost(t *testing.T) {
	var n NullReporter
	if err := n.OnEvent(context.Background(), Event{State: StateScouting}); err != nil {
		t.Errorf("OnEvent should be nil: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Errorf("Close should be nil: %v", err)
	}
}

func TestWithReporterOptionReplacesDefault(t *testing.T) {
	rec := &recordingReporter{}
	o := &Orchestrator{}
	WithReporter(rec)(o)
	if o.reporter == nil {
		t.Fatal("reporter not set")
	}
	if _, ok := o.reporter.(*recordingReporter); !ok {
		t.Errorf("reporter type = %T, want *recordingReporter", o.reporter)
	}
}

func TestWithReporterNilIgnored(t *testing.T) {
	o := &Orchestrator{reporter: NullReporter{}}
	WithReporter(nil)(o)
	if _, ok := o.reporter.(NullReporter); !ok {
		t.Errorf("nil reporter should be ignored, got %T", o.reporter)
	}
}

func TestSetReporterReplacesInPlace(t *testing.T) {
	o := &Orchestrator{reporter: NullReporter{}}
	rec := &recordingReporter{}
	o.SetReporter(rec)
	if _, ok := o.reporter.(*recordingReporter); !ok {
		t.Errorf("SetReporter did not replace: %T", o.reporter)
	}
	o.SetReporter(nil)
	if _, ok := o.reporter.(NullReporter); !ok {
		t.Errorf("SetReporter(nil) should reset to NullReporter, got %T", o.reporter)
	}
}

// TestEmitFansOutToChannelAndReporter verifies the core contract: every
// event emitted by the orchestrator goes to BOTH the TUI channel and the
// Reporter, in that order, with the agent context attached.
func TestEmitFansOutToChannelAndReporter(t *testing.T) {
	rec := &recordingReporter{}
	o := &Orchestrator{
		ctx:         &agents.Context{ScoutReport: "brief"},
		reporter:    rec,
		reporterLog: discardWriter{},
	}
	out := make(chan Event, 2)
	o.emit(context.Background(), out, Event{State: StateScouting, Message: "go"})
	close(out)

	received := <-out
	if received.State != StateScouting || received.Message != "go" {
		t.Errorf("channel got wrong event: %+v", received)
	}
	if received.Report == nil || received.Report.ScoutReport != "brief" {
		t.Errorf("context not stamped on event: %+v", received.Report)
	}

	if len(rec.events) != 1 {
		t.Fatalf("reporter got %d events, want 1", len(rec.events))
	}
	if rec.events[0].State != StateScouting {
		t.Errorf("reporter state = %q, want scouting", rec.events[0].State)
	}
	if rec.events[0].Report == nil || rec.events[0].Report.ScoutReport != "brief" {
		t.Errorf("reporter did not see context: %+v", rec.events[0].Report)
	}
}

// TestEmitSwallowsReporterErrors verifies that a reporter returning an
// error does not block the channel send and does not propagate.
func TestEmitSwallowsReporterErrors(t *testing.T) {
	rec := &recordingReporter{fail: errors.New("network down")}
	var logBuf strings.Builder
	o := &Orchestrator{
		ctx:         &agents.Context{},
		reporter:    rec,
		reporterLog: &logBuf,
	}
	out := make(chan Event, 1)
	o.emit(context.Background(), out, Event{State: StateError, Err: errors.New("x")})
	close(out)

	if _, ok := <-out; !ok {
		t.Fatal("channel did not receive event despite reporter error")
	}
	if !strings.Contains(logBuf.String(), "network down") {
		t.Errorf("reporter error not logged: %q", logBuf.String())
	}
}

// discardWriter is a tiny helper to give the orchestrator a bit-bucket
// log sink in tests without pulling in io/ioutil.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
