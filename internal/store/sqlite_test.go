package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harsha/relay/internal/model"
	"github.com/harsha/relay/internal/store"
)

func newTestStore(t *testing.T) *store.SQLite {
	t.Helper()
	db, _ := newTestStoreWithPath(t)
	return db
}

func newTestStoreWithPath(t *testing.T) (*store.SQLite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func seedTask(t *testing.T, ctx context.Context, db *store.SQLite, id string, updatedAt time.Time) {
	t.Helper()
	task := model.Task{ID: id, Repository: "/repo", Objective: "objective for " + id, State: model.TaskCompleted, Version: 1, CreatedAt: updatedAt, UpdatedAt: updatedAt}
	event := model.Event{ID: "evt-" + id, TaskID: id, Sequence: 1, Type: "task.created", Actor: "user", Summary: task.Objective, CreatedAt: updatedAt}
	if err := db.CreateTask(ctx, task, event); err != nil {
		t.Fatal(err)
	}
	record := model.RunRecord{ID: "run-" + id, TaskID: id, Adapter: "test", SessionID: "session-" + id, Status: "COMPLETED", StartedAt: updatedAt, CompletedAt: updatedAt}
	if err := db.RecordRun(ctx, record, task.Repository); err != nil {
		t.Fatal(err)
	}
}

func TestPruneOldRunsDeletesOnlyStaleTaskHistory(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	now := time.Now().UTC()
	seedTask(t, ctx, db, "stale", now.Add(-8*24*time.Hour))
	seedTask(t, ctx, db, "fresh", now.Add(-1*time.Hour))

	pruned, err := db.PruneOldRuns(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if _, err := db.Task(ctx, "stale"); err != store.ErrNotFound {
		t.Fatalf("stale task err = %v, want ErrNotFound", err)
	}
	if _, err := db.Task(ctx, "fresh"); err != nil {
		t.Fatalf("fresh task should survive pruning: %v", err)
	}
	events, err := db.Events(ctx, "fresh")
	if err != nil || len(events) != 1 {
		t.Fatalf("fresh events = %+v, err = %v", events, err)
	}
}

func TestPruneOldRunsRemovesChildrenAndKeepsMemory(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	now := time.Now().UTC()
	seedTask(t, ctx, db, "old", now.Add(-30*24*time.Hour))
	if err := db.ApplyMemory(ctx, "/repo", "old", model.MemoryUpdate{Upsert: []model.MemoryEntry{{Key: "durable.key", Value: "kept"}}}, now); err != nil {
		t.Fatal(err)
	}

	if _, err := db.PruneOldRuns(ctx, now.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if events, err := db.Events(ctx, "old"); err != nil || len(events) != 0 {
		t.Fatalf("events of pruned task = %+v, err = %v", events, err)
	}
	items, err := db.ProjectMemory(ctx, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "durable.key" || items[0].Value != "kept" {
		t.Fatalf("memory = %+v, want durable memory preserved", items)
	}
}

func TestPruneOldRunsOnEmptyDatabase(t *testing.T) {
	db := newTestStore(t)
	pruned, err := db.PruneOldRuns(context.Background(), time.Now().UTC())
	if err != nil || pruned != 0 {
		t.Fatalf("pruned = %d, err = %v, want 0, nil", pruned, err)
	}
}

func TestTaskReturnsTimestampParseError(t *testing.T) {
	ctx := context.Background()
	db, path := newTestStoreWithPath(t)
	seedTask(t, ctx, db, "bad-task-time", time.Now().UTC())
	corruptTimestamp(t, path, `UPDATE tasks SET created_at='not-a-time' WHERE id='bad-task-time'`)

	_, err := db.Task(ctx, "bad-task-time")
	if err == nil || !strings.Contains(err.Error(), "parse task created_at") {
		t.Fatalf("Task err = %v, want task timestamp parse error", err)
	}
}

func TestEventsReturnsTimestampParseError(t *testing.T) {
	ctx := context.Background()
	db, path := newTestStoreWithPath(t)
	seedTask(t, ctx, db, "bad-event-time", time.Now().UTC())
	corruptTimestamp(t, path, `UPDATE events SET created_at='not-a-time' WHERE task_id='bad-event-time'`)

	_, err := db.Events(ctx, "bad-event-time")
	if err == nil || !strings.Contains(err.Error(), "parse event created_at") {
		t.Fatalf("Events err = %v, want event timestamp parse error", err)
	}
}

func TestProjectMemoryReturnsTimestampParseError(t *testing.T) {
	ctx := context.Background()
	db, path := newTestStoreWithPath(t)
	now := time.Now().UTC()
	seedTask(t, ctx, db, "bad-memory-time", now)
	if err := db.ApplyMemory(ctx, "/repo", "bad-memory-time", model.MemoryUpdate{Upsert: []model.MemoryEntry{{Key: "bad.time", Value: "value"}}}, now); err != nil {
		t.Fatal(err)
	}
	corruptTimestamp(t, path, `UPDATE project_memory SET updated_at='not-a-time' WHERE repository='/repo' AND key='bad.time'`)

	_, err := db.ProjectMemory(ctx, "/repo")
	if err == nil || !strings.Contains(err.Error(), "parse project memory updated_at") {
		t.Fatalf("ProjectMemory err = %v, want memory timestamp parse error", err)
	}
}

func corruptTimestamp(t *testing.T, path, query string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(query); err != nil {
		t.Fatal(err)
	}
}
