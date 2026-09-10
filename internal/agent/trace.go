package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TraceEventType 遵循 AG-UI 的生命周期语义，同时保留重放一次完整
// ReAct run 所需的额外事件。
type TraceEventType string

const (
	TraceSessionStarted       TraceEventType = "SESSION_STARTED"
	TraceSessionEnded         TraceEventType = "SESSION_ENDED"
	TracePromptQueued         TraceEventType = "PROMPT_QUEUED"
	TracePromptDequeued       TraceEventType = "PROMPT_DEQUEUED"
	TraceRunStarted           TraceEventType = "RUN_STARTED"
	TraceRunFinished          TraceEventType = "RUN_FINISHED"
	TraceModelRequestStarted  TraceEventType = "MODEL_REQUEST_STARTED"
	TraceModelRequestFinished TraceEventType = "MODEL_REQUEST_FINISHED"
	TraceTextMessageContent   TraceEventType = "TEXT_MESSAGE_CONTENT"
	TraceThinkingContent      TraceEventType = "THINKING_CONTENT"
	TraceToolCallStarted      TraceEventType = "TOOL_CALL_START"
	TraceToolCallArguments    TraceEventType = "TOOL_CALL_ARGS"
	TraceToolCallFinished     TraceEventType = "TOOL_CALL_END"
	TraceToolExecutionStarted TraceEventType = "TOOL_EXECUTION_STARTED"
	TraceToolExecutionEnded   TraceEventType = "TOOL_EXECUTION_ENDED"
	TraceContextCompacted     TraceEventType = "CONTEXT_COMPACTED"
	TraceMemoryRecalled       TraceEventType = "MEMORY_RECALLED"
	TraceRetry                TraceEventType = "RETRY"
	TraceError                TraceEventType = "RUN_ERROR"
)

type TraceSource string

const (
	TraceSourceSession TraceSource = "session"
	TraceSourceRun     TraceSource = "run"
	TraceSourceModel   TraceSource = "model"
	TraceSourceTool    TraceSource = "tool"
	TraceSourceContext TraceSource = "context"
	TraceSourceMemory  TraceSource = "memory"
)

type RunFinishReason string

const (
	RunCompleted        RunFinishReason = "completed"
	RunFailed           RunFinishReason = "failed"
	RunCancelled        RunFinishReason = "cancelled"
	RunContextCompacted RunFinishReason = "context_compacted"
	RunMaxIterations    RunFinishReason = "max_iterations"
	RunUserRejected     RunFinishReason = "user_rejected"
)

// TraceEvent 是与 provider 无关的、只追加的记录，用于实时订阅者和持久化。
// Payload 里放的是摘要/引用，
// 而不是不加限制的工具输出。
type TraceEvent struct {
	Type       TraceEventType  `json:"type"`
	Source     TraceSource     `json:"source"`
	Timestamp  time.Time       `json:"timestamp"`
	Sequence   uint64          `json:"sequence"`
	SessionID  string          `json:"session_id,omitempty"`
	RunID      string          `json:"run_id,omitempty"`
	Iteration  int             `json:"iteration,omitempty"`
	RequestID  string          `json:"request_id,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	Reason     RunFinishReason `json:"reason,omitempty"`
	Payload    map[string]any  `json:"payload,omitempty"`
}

// TraceSink 接收已经编好序号的 trace 事件。
type TraceSink interface {
	AppendTrace(context.Context, TraceEvent) error
}

type TraceSequenceLoader interface {
	LastTraceSequence(sessionID, runID string) (uint64, error)
}

// TraceRecorder 在每次 run 内部把事件串行化，并扇出给实时订阅者。
// 慢订阅者永远不会阻塞 AgentRun。
type TraceRecorder struct {
	mu          sync.Mutex
	sink        TraceSink
	next        map[string]uint64
	subscribers map[uint64]*traceSubscriber
	nextSubID   uint64
}

type traceSubscriber struct {
	in   chan TraceEvent
	out  chan TraceEvent
	stop chan struct{}
}

func NewTraceRecorder(sink TraceSink) *TraceRecorder {
	return &TraceRecorder{
		sink:        sink,
		next:        make(map[string]uint64),
		subscribers: make(map[uint64]*traceSubscriber),
	}
}

func (r *TraceRecorder) Record(ctx context.Context, event TraceEvent) TraceEvent {
	if r == nil {
		return event
	}
	r.mu.Lock()
	key := event.SessionID + "\x00" + event.RunID
	if _, initialized := r.next[key]; !initialized {
		if loader, ok := r.sink.(TraceSequenceLoader); ok {
			if last, err := loader.LastTraceSequence(event.SessionID, event.RunID); err == nil {
				r.next[key] = last
			}
		}
	}
	r.next[key]++
	event.Sequence = r.next[key]
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if r.sink != nil {
		_ = r.sink.AppendTrace(ctx, event)
	}
	for _, subscriber := range r.subscribers {
		subscriber.in <- event
	}
	r.mu.Unlock()
	return event
}

func (r *TraceRecorder) Subscribe(buffer int) (<-chan TraceEvent, func()) {
	if buffer < 1 {
		buffer = 64
	}
	subscriber := newTraceSubscriber(buffer)
	r.mu.Lock()
	r.nextSubID++
	id := r.nextSubID
	r.subscribers[id] = subscriber
	r.mu.Unlock()
	var once sync.Once
	return subscriber.out, func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subscribers, id)
			close(subscriber.stop)
			r.mu.Unlock()
		})
	}
}

func newTraceSubscriber(buffer int) *traceSubscriber {
	subscriber := &traceSubscriber{
		in:   make(chan TraceEvent),
		out:  make(chan TraceEvent, buffer),
		stop: make(chan struct{}),
	}
	go func() {
		defer close(subscriber.out)
		var queue []TraceEvent
		for {
			var out chan TraceEvent
			var first TraceEvent
			if len(queue) > 0 {
				out = subscriber.out
				first = queue[0]
			}
			select {
			case <-subscriber.stop:
				return
			case event := <-subscriber.in:
				queue = append(queue, event)
			case out <- first:
				queue = queue[1:]
			}
		}
	}()
	return subscriber
}

// MemoryTraceStore 适用于测试，也适用于之后
// 会自带持久化存储的嵌入方。
type MemoryTraceStore struct {
	mu     sync.Mutex
	events []TraceEvent
}

func (s *MemoryTraceStore) AppendTrace(_ context.Context, event TraceEvent) error {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	return nil
}

func (s *MemoryTraceStore) Events(sessionID, runID string) []TraceEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TraceEvent
	for _, event := range s.events {
		if (sessionID == "" || event.SessionID == sessionID) && (runID == "" || event.RunID == runID) {
			out = append(out, event)
		}
	}
	return out
}

func (s *MemoryTraceStore) LastTraceSequence(sessionID, runID string) (uint64, error) {
	events := s.Events(sessionID, runID)
	var last uint64
	for _, event := range events {
		if event.Sequence > last {
			last = event.Sequence
		}
	}
	return last, nil
}

// JSONLTraceStore 为每次 run 持久化一个只追加的 JSONL 文件。
// 单个 store 在并发 session 下是安全的。
type JSONLTraceStore struct {
	Root string
	mu   sync.Mutex
}

func NewJSONLTraceStore(workDir string) *JSONLTraceStore {
	return &JSONLTraceStore{Root: filepath.Join(workDir, ".mewcode", "traces")}
}

func (s *JSONLTraceStore) AppendTrace(_ context.Context, event TraceEvent) error {
	if s == nil || s.Root == "" || event.SessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.Root, filepath.Base(event.SessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fileID := event.RunID
	if fileID == "" {
		fileID = "_session"
	}
	f, err := os.OpenFile(filepath.Join(dir, filepath.Base(fileID)+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(event)
}

func (s *JSONLTraceStore) Replay(sessionID, runID string) ([]TraceEvent, error) {
	if s == nil || s.Root == "" {
		return nil, errors.New("trace store is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID == "" {
		runID = "_session"
	}
	f, err := os.Open(filepath.Join(s.Root, filepath.Base(sessionID), filepath.Base(runID)+".jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []TraceEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event TraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}

func (s *JSONLTraceStore) LastTraceSequence(sessionID, runID string) (uint64, error) {
	events, err := s.Replay(sessionID, runID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var last uint64
	for _, event := range events {
		if event.Sequence > last {
			last = event.Sequence
		}
	}
	return last, nil
}

const tracePreviewLimit = 2048

func tracePreview(value string) (preview string, truncated bool) {
	if len(value) <= tracePreviewLimit {
		return value, false
	}
	return value[:tracePreviewLimit], true
}
