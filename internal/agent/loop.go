package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"mewcode/internal/conversation"
	"mewcode/internal/hooks"
)

var (
	ErrSessionExists   = errors.New("agent loop session already exists")
	ErrSessionNotFound = errors.New("agent loop session not found")
	ErrAgentInUse      = errors.New("agent instance is already owned by another session")
)

type QueueStatus string

const (
	QueueQueued  QueueStatus = "queued"
	QueueRunning QueueStatus = "running"
	QueueDone    QueueStatus = "done"
)

// QueueStatusEvent 让繁忙会话的行为对每个前端都可见。
type QueueStatusEvent struct {
	SessionID string
	RunID     string
	Position  int
	Status    QueueStatus
}

func (QueueStatusEvent) agentEvent() {}

type RunResult struct {
	SessionID string
	RunID     string
	Reason    RunFinishReason
	Err       error
}

type Submission struct {
	SessionID string
	RunID     string
	Position  int
	Events    <-chan AgentEvent
	Done      <-chan RunResult
}

type SessionOptions struct {
	Agent        *Agent
	Conversation *conversation.Manager
	// BeforeRun 在会话 worker 上执行，位置是给对话做快照之前。
	// 要做会话级的记忆召回，或者只想给 provider 下一次指令，
	// 放在这里正合适。
	BeforeRun  func(context.Context, string, string, *Agent, *conversation.Manager)
	AfterRun   func(RunResult, *conversation.Manager)
	QueueStore QueueStore
}

type queuedPrompt struct {
	id      string
	prompt  string
	ctx     context.Context
	eventIn chan AgentEvent
	events  <-chan AgentEvent
	done    chan RunResult
}

type loopSession struct {
	id      string
	options SessionOptions
	context ContextCoordinator

	mu           sync.Mutex
	queue        []*queuedPrompt
	signal       chan struct{}
	stop         chan struct{}
	activeCancel context.CancelFunc
	running      bool
}

// AgentLoop 是会话调度层。每个已注册的会话各有一个 worker（严格 FIFO）；
// 不同会话的 worker 之间并发执行。
type AgentLoop struct {
	mu       sync.Mutex
	sessions map[string]*loopSession
	trace    *TraceRecorder
}

func NewAgentLoop(trace *TraceRecorder) *AgentLoop {
	return &AgentLoop{sessions: make(map[string]*loopSession), trace: trace}
}

func (l *AgentLoop) RegisterSession(sessionID string, options SessionOptions) error {
	if sessionID == "" || options.Agent == nil {
		return errors.New("session id and agent are required")
	}
	if options.Conversation == nil {
		options.Conversation = conversation.NewManager()
	}
	var restored []QueuedPrompt
	if options.QueueStore != nil {
		restored, _ = options.QueueStore.LoadQueue(sessionID)
	}
	l.mu.Lock()
	if _, exists := l.sessions[sessionID]; exists {
		l.mu.Unlock()
		return ErrSessionExists
	}
	for _, existing := range l.sessions {
		if existing.options.Agent == options.Agent {
			l.mu.Unlock()
			return ErrAgentInUse
		}
	}
	state := &loopSession{
		id:      sessionID,
		options: options,
		context: &contextCoordinator{},
		signal:  make(chan struct{}, 1),
		stop:    make(chan struct{}),
	}
	for _, saved := range restored {
		if saved.RunID == "" {
			saved.RunID = newRuntimeID("run")
		}
		eventIn, events := newAgentEventPipe()
		item := &queuedPrompt{
			id: saved.RunID, prompt: saved.Prompt, ctx: context.Background(),
			eventIn: eventIn, events: events, done: make(chan RunResult, 1),
		}
		state.queue = append(state.queue, item)
		go func(item *queuedPrompt) {
			for range item.events {
			}
			<-item.done
		}(item)
	}
	l.sessions[sessionID] = state
	l.mu.Unlock()
	options.Agent.emitHook(hooks.EventSessionStart, "", nil)
	if l.trace != nil {
		l.trace.Record(context.Background(), TraceEvent{Type: TraceSessionStarted, Source: TraceSourceSession, SessionID: sessionID})
	}
	go l.runSession(state)
	if len(restored) > 0 {
		for i, item := range state.queue {
			item.eventIn <- QueueStatusEvent{SessionID: sessionID, RunID: item.id, Position: i + 1, Status: QueueQueued}
			if l.trace != nil {
				l.trace.Record(context.Background(), TraceEvent{Type: TracePromptQueued, Source: TraceSourceSession, SessionID: sessionID, RunID: item.id, Payload: map[string]any{"position": i + 1, "restored": true}})
			}
		}
		state.signal <- struct{}{}
	}
	return nil
}

// ReplaceSession 供 resume/clear 使用，调用方需先确认旧会话没有正在跑的 run。
// 它不会影响其他会话。
func (l *AgentLoop) ReplaceSession(sessionID string, options SessionOptions) error {
	_ = l.UnregisterSession(sessionID)
	return l.RegisterSession(sessionID, options)
}

func (l *AgentLoop) UnregisterSession(sessionID string) error {
	l.mu.Lock()
	state, ok := l.sessions[sessionID]
	if ok {
		delete(l.sessions, sessionID)
	}
	l.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}
	state.mu.Lock()
	if state.activeCancel != nil {
		state.activeCancel()
	}
	close(state.stop)
	for _, item := range state.queue {
		close(item.eventIn)
		item.done <- RunResult{SessionID: sessionID, RunID: item.id, Reason: RunCancelled, Err: context.Canceled}
		close(item.done)
	}
	state.queue = nil
	state.mu.Unlock()
	state.options.Agent.emitHook(hooks.EventSessionEnd, "", nil)
	if l.trace != nil {
		l.trace.Record(context.Background(), TraceEvent{Type: TraceSessionEnded, Source: TraceSourceSession, SessionID: sessionID})
	}
	return nil
}

func (l *AgentLoop) Submit(ctx context.Context, sessionID, prompt string) (Submission, error) {
	l.mu.Lock()
	state, ok := l.sessions[sessionID]
	l.mu.Unlock()
	if !ok {
		return Submission{}, ErrSessionNotFound
	}
	return l.enqueue(state, ctx, newRuntimeID("run"), prompt), nil
}

func (l *AgentLoop) enqueue(state *loopSession, ctx context.Context, runID, prompt string) Submission {
	if ctx == nil {
		ctx = context.Background()
	}
	eventIn, events := newAgentEventPipe()
	item := &queuedPrompt{
		id:      runID,
		prompt:  prompt,
		ctx:     ctx,
		eventIn: eventIn,
		events:  events,
		done:    make(chan RunResult, 1),
	}
	state.mu.Lock()
	position := len(state.queue) + 1
	if state.running {
		position++
	}
	state.queue = append(state.queue, item)
	state.persistQueueLocked()
	state.mu.Unlock()
	item.eventIn <- QueueStatusEvent{SessionID: state.id, RunID: runID, Position: position, Status: QueueQueued}
	if l.trace != nil {
		l.trace.Record(ctx, TraceEvent{Type: TracePromptQueued, Source: TraceSourceSession, SessionID: state.id, RunID: runID, Payload: map[string]any{"position": position}})
	}
	select {
	case state.signal <- struct{}{}:
	default:
	}
	return Submission{SessionID: state.id, RunID: runID, Position: position, Events: item.events, Done: item.done}
}

func (l *AgentLoop) Cancel(sessionID string) error {
	l.mu.Lock()
	state, ok := l.sessions[sessionID]
	l.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}
	state.mu.Lock()
	if state.activeCancel != nil {
		state.activeCancel()
	}
	state.mu.Unlock()
	return nil
}

func (l *AgentLoop) QueueDepth(sessionID string) (int, bool) {
	l.mu.Lock()
	state, ok := l.sessions[sessionID]
	l.mu.Unlock()
	if !ok {
		return 0, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.queue), true
}

func (l *AgentLoop) IsBusy(sessionID string) bool {
	l.mu.Lock()
	state, ok := l.sessions[sessionID]
	l.mu.Unlock()
	if !ok {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.running || len(state.queue) > 0
}

func (l *AgentLoop) runSession(state *loopSession) {
	for {
		select {
		case <-state.stop:
			return
		case <-state.signal:
		}
		for {
			state.mu.Lock()
			if len(state.queue) == 0 {
				state.running = false
				state.mu.Unlock()
				break
			}
			item := state.queue[0]
			state.queue = state.queue[1:]
			state.running = true
			state.persistQueueLocked()
			runCtx, cancel := context.WithCancel(item.ctx)
			state.activeCancel = cancel
			state.mu.Unlock()

			item.eventIn <- QueueStatusEvent{SessionID: state.id, RunID: item.id, Status: QueueRunning}
			if l.trace != nil {
				l.trace.Record(runCtx, TraceEvent{Type: TracePromptDequeued, Source: TraceSourceSession, SessionID: state.id, RunID: item.id})
			}
			if state.options.BeforeRun != nil {
				state.options.BeforeRun(runCtx, state.id, item.id, state.options.Agent, state.options.Conversation)
			}

			working := state.options.Conversation.Clone()
			run := state.options.Agent.NewRun(RunOptions{
				ID: item.id, SessionID: state.id, Prompt: item.prompt,
				Conversation: working, Trace: l.trace, Context: state.context,
			})
			var runErr error
			// 每次独立的user请求都封装为一次run，run.Start(runCtx)负责处理这个独立的用户请求
			for event := range run.Start(runCtx) {
				switch e := event.(type) {
				case ErrorEvent:
					runErr = errors.New(e.Message)
				}
				item.eventIn <- event
			}
			reason := run.FinishReason()
			if reason == "" {
				reason = RunFailed
			}
			if runCtx.Err() != nil {
				runErr = runCtx.Err()
			}
			cancel()
			state.options.Conversation.ReplaceWith(working)
			result := RunResult{SessionID: state.id, RunID: item.id, Reason: reason, Err: runErr}
			item.eventIn <- QueueStatusEvent{SessionID: state.id, RunID: item.id, Status: QueueDone}
			close(item.eventIn)
			item.done <- result
			close(item.done)
			if state.options.AfterRun != nil {
				state.options.AfterRun(result, state.options.Conversation)
			}
			state.mu.Lock()
			state.activeCancel = nil
			state.mu.Unlock()
		}
	}
}

// newAgentEventPipe 把 run 的执行与前端消费解耦，同时保住每一个有序的流事件。
// 消费慢的订阅者不会拖住一次 run。
func newAgentEventPipe() (chan AgentEvent, <-chan AgentEvent) {
	in := make(chan AgentEvent)
	out := make(chan AgentEvent)
	go func() {
		defer close(out)
		var queue []AgentEvent
		for in != nil || len(queue) > 0 {
			var outCh chan AgentEvent
			var first AgentEvent
			if len(queue) > 0 {
				outCh = out
				first = queue[0]
			}
			select {
			case event, ok := <-in:
				if !ok {
					in = nil
					continue
				}
				queue = append(queue, event)
			case outCh <- first:
				queue[0] = nil
				queue = queue[1:]
			}
		}
	}()
	return in, out
}

type QueuedPrompt struct {
	RunID  string `json:"run_id"`
	Prompt string `json:"prompt"`
}

type QueueStore interface {
	SaveQueue(sessionID string, prompts []QueuedPrompt) error
	LoadQueue(sessionID string) ([]QueuedPrompt, error)
}

// JSONQueueStore 让 run 进行期间到达的 prompt 在进程重启后仍可恢复。
// 文件权限设为仅用户可读写，因为 prompt 可能包含敏感内容。
type JSONQueueStore struct{ Root string }

func NewJSONQueueStore(workDir string) *JSONQueueStore {
	return &JSONQueueStore{Root: filepath.Join(workDir, ".mewcode", "queues")}
}

func (s *JSONQueueStore) SaveQueue(sessionID string, prompts []QueuedPrompt) error {
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.Root, filepath.Base(sessionID)+".json")
	if len(prompts) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(prompts)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Root, filepath.Base(sessionID)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (s *JSONQueueStore) LoadQueue(sessionID string) ([]QueuedPrompt, error) {
	data, err := os.ReadFile(filepath.Join(s.Root, filepath.Base(sessionID)+".json"))
	if err != nil {
		return nil, err
	}
	var prompts []QueuedPrompt
	if err := json.Unmarshal(data, &prompts); err != nil {
		return nil, err
	}
	return prompts, nil
}

func (s *loopSession) persistQueueLocked() {
	if s.options.QueueStore == nil {
		return
	}
	prompts := make([]QueuedPrompt, 0, len(s.queue))
	for _, item := range s.queue {
		prompts = append(prompts, QueuedPrompt{RunID: item.id, Prompt: item.prompt})
	}
	_ = s.options.QueueStore.SaveQueue(s.id, prompts)
}
