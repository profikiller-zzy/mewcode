package teams

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *SharedTaskStore {
	t.Helper()
	return NewSharedTaskStore(filepath.Join(t.TempDir(), "tasks.json"))
}

func strptr(s string) *string { return &s }

func TestSharedTaskCreateAssignsStringIDsAndPending(t *testing.T) {
	store := newTestStore(t)
	t1 := store.Create("first", "", "", nil, nil, "lead")
	t2 := store.Create("second", "desc", "alice", nil, nil, "lead")

	if t1.ID != "1" || t2.ID != "2" {
		t.Fatalf("ids = %q,%q, want 1,2", t1.ID, t2.ID)
	}
	if t1.Status != "pending" {
		t.Fatalf("status = %q, want pending", t1.Status)
	}
	if t2.Assignee != "alice" || t2.Description != "desc" || t2.CreatedBy != "lead" {
		t.Fatalf("unexpected task2 fields: %+v", t2)
	}
}

func TestSharedTaskGetAndList(t *testing.T) {
	store := newTestStore(t)
	store.Create("a", "", "alice", nil, nil, "")
	b := store.Create("b", "", "bob", nil, nil, "")
	store.Update(b.ID, TaskUpdate{Status: strptr("completed")})

	if store.Get("999") != nil {
		t.Fatalf("get missing should be nil")
	}
	if len(store.ListTasks("", "")) != 2 {
		t.Fatalf("list all should be 2")
	}
	if len(store.ListTasks("completed", "")) != 1 {
		t.Fatalf("filter by status failed")
	}
	if len(store.ListTasks("", "alice")) != 1 {
		t.Fatalf("filter by assignee failed")
	}
	if len(store.ListTasks("completed", "alice")) != 0 {
		t.Fatalf("combined filter failed")
	}
}

func TestSharedTaskUpdateAndDeps(t *testing.T) {
	store := newTestStore(t)
	task := store.Create("task", "", "", nil, nil, "")
	updated := store.Update(task.ID, TaskUpdate{
		Status:       strptr("in_progress"),
		Assignee:     strptr("carol"),
		Description:  strptr("new desc"),
		AddBlocks:    []string{"2"},
		AddBlockedBy: []string{"3"},
	})
	if updated == nil || updated.Status != "in_progress" || updated.Assignee != "carol" {
		t.Fatalf("update result unexpected: %+v", updated)
	}
	if len(updated.Blocks) != 1 || updated.Blocks[0] != "2" {
		t.Fatalf("blocks not appended: %+v", updated.Blocks)
	}
	// 重复追加去重
	again := store.Update(task.ID, TaskUpdate{AddBlocks: []string{"2"}})
	if len(again.Blocks) != 1 {
		t.Fatalf("dedup failed: %+v", again.Blocks)
	}
	if store.Update("nope", TaskUpdate{Status: strptr("completed")}) != nil {
		t.Fatalf("update missing should be nil")
	}
}

func TestSharedTaskPersistenceAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	store1 := NewSharedTaskStore(path)
	store1.Create("persisted", "", "", nil, nil, "lead")

	// 另一个实例（模拟队友进程）读同一份文件
	store2 := NewSharedTaskStore(path)
	if len(store2.ListTasks("", "")) != 1 {
		t.Fatalf("store2 should see 1 task")
	}
	// store2 写入后，store1 读前会 reload
	store2.Create("from-teammate", "", "", nil, nil, "bob")
	if got := store1.Get("2"); got == nil || got.Title != "from-teammate" {
		t.Fatalf("store1 did not reload teammate task: %+v", got)
	}
}

func TestSharedTaskInitEmpty(t *testing.T) {
	store := newTestStore(t)
	store.Create("x", "", "", nil, nil, "")
	store.InitEmpty()
	if len(store.ListTasks("", "")) != 0 {
		t.Fatalf("initEmpty did not clear")
	}
	if store.Create("y", "", "", nil, nil, "").ID != "1" {
		t.Fatalf("nextID not reset")
	}
}

func TestSharedTaskConcurrentCreateAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	const workers = 8
	const perWorker = 25
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			store := NewSharedTaskStore(path)
			for j := 0; j < perWorker; j++ {
				if task, err := store.CreateWithError("task", "", "", nil, nil, "worker"); err != nil || task.ID == "" {
					t.Errorf("worker %d create failed: task=%+v err=%v", worker, task, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	store := NewSharedTaskStore(path)
	tasks := store.ListTasks("", "")
	if got, want := len(tasks), workers*perWorker; got != want {
		t.Fatalf("concurrent creates lost tasks: got %d, want %d", got, want)
	}
	seen := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		if seen[task.ID] {
			t.Fatalf("duplicate task id %q", task.ID)
		}
		seen[task.ID] = true
	}
}

func TestSharedTaskWriteRejectsCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewSharedTaskStore(path)
	if _, err := store.CreateWithError("should fail", "", "", nil, nil, "lead"); err == nil {
		t.Fatal("expected corrupt task board to return an error")
	}
}

func TestSharedTaskConcurrentCreateAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	const workers = 4
	const perWorker = 15
	cmds := make([]*exec.Cmd, 0, workers)
	for i := 0; i < workers; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=TestSharedTaskCreateHelper", "--")
		cmd.Env = append(os.Environ(),
			"MEWCODE_TASK_HELPER=1",
			"MEWCODE_TASK_PATH="+path,
			"MEWCODE_TASK_COUNT="+strconv.Itoa(perWorker),
		)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper process failed: %v", err)
		}
	}

	tasks := NewSharedTaskStore(path).ListTasks("", "")
	if got, want := len(tasks), workers*perWorker; got != want {
		t.Fatalf("cross-process creates lost tasks: got %d, want %d", got, want)
	}
}

func TestSharedTaskCreateHelper(t *testing.T) {
	if os.Getenv("MEWCODE_TASK_HELPER") != "1" {
		return
	}
	count, err := strconv.Atoi(os.Getenv("MEWCODE_TASK_COUNT"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewSharedTaskStore(os.Getenv("MEWCODE_TASK_PATH"))
	for i := 0; i < count; i++ {
		if _, err := store.CreateWithError("task", "", "", nil, nil, "process"); err != nil {
			t.Fatal(err)
		}
	}
}
