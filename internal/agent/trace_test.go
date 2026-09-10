package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestJSONLTraceStoreReplayAndLiveSubscription(t *testing.T) {
	store := NewJSONLTraceStore(t.TempDir())
	recorder := NewTraceRecorder(store)
	live, unsubscribe := recorder.Subscribe(4)
	recorder.Record(context.Background(), TraceEvent{Type: TraceRunStarted, Source: TraceSourceRun, SessionID: "s1", RunID: "r1"})
	recorder.Record(context.Background(), TraceEvent{Type: TraceRunFinished, Source: TraceSourceRun, SessionID: "s1", RunID: "r1", Reason: RunCompleted})

	for want := uint64(1); want <= 2; want++ {
		select {
		case event := <-live:
			if event.Sequence != want {
				t.Fatalf("live sequence = %d, want %d", event.Sequence, want)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for live trace")
		}
	}
	unsubscribe()

	events, err := store.Replay("s1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != TraceRunStarted || events[1].Reason != RunCompleted {
		t.Fatalf("unexpected replay: %+v", events)
	}
	// A recorder recreated after process restart continues the persisted
	// sequence instead of appending a second sequence=1.
	restarted := NewTraceRecorder(store)
	event := restarted.Record(context.Background(), TraceEvent{Type: TraceRetry, Source: TraceSourceRun, SessionID: "s1", RunID: "r1"})
	if event.Sequence != 3 {
		t.Fatalf("restarted sequence = %d, want 3", event.Sequence)
	}
	info, err := os.Stat(store.Root + "/s1/r1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("trace file is not private: mode=%o", info.Mode().Perm())
	}
}

func TestJSONQueueStoreRoundTrip(t *testing.T) {
	store := NewJSONQueueStore(t.TempDir())
	want := []QueuedPrompt{{RunID: "r1", Prompt: "first"}, {RunID: "r2", Prompt: "second"}}
	if err := store.SaveQueue("s1", want); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadQueue("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("queue round-trip = %+v", got)
	}
	if err := store.SaveQueue("s1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadQueue("s1"); !os.IsNotExist(err) {
		t.Fatalf("empty queue should remove persisted state, got %v", err)
	}
}
