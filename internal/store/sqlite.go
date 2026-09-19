package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/harsha/relay/internal/model"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type SQLite struct{ db *sql.DB }

func Open(path string) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	// SQLite connection-local pragmas and write serialization are predictable
	// when the MVP uses one connection. Revisit this when the daemon introduces
	// concurrent readers and a dedicated writer.
	db.SetMaxOpenConns(1)
	s := &SQLite{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS repositories (
  path TEXT PRIMARY KEY,
  last_seen_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  repository TEXT NOT NULL,
  objective TEXT NOT NULL,
  state TEXT NOT NULL,
  version INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_repo_updated ON tasks(repository, updated_at DESC);
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  sequence INTEGER NOT NULL,
  type TEXT NOT NULL,
  actor TEXT NOT NULL,
  summary TEXT NOT NULL,
  data_json TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(task_id, sequence)
);
CREATE INDEX IF NOT EXISTS events_task_sequence ON events(task_id, sequence);
CREATE TABLE IF NOT EXISTS sessions (
  adapter TEXT NOT NULL,
  upstream_id TEXT NOT NULL,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  repository TEXT NOT NULL,
  healthy INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  last_used_at TEXT NOT NULL,
  PRIMARY KEY(adapter, upstream_id)
);
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  adapter TEXT NOT NULL,
  session_id TEXT,
  status TEXT NOT NULL,
  exit_code INTEGER NOT NULL,
  response TEXT,
  usage_json TEXT,
  started_at TEXT NOT NULL,
  completed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_task_started ON runs(task_id, started_at);
`)
	if err != nil {
		return fmt.Errorf("migrate state: %w", err)
	}
	return nil
}

func (s *SQLite) CreateTask(ctx context.Context, task model.Task, event model.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO repositories(path,last_seen_at) VALUES(?,?) ON CONFLICT(path) DO UPDATE SET last_seen_at=excluded.last_seen_at`, task.Repository, stamp(task.CreatedAt)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,repository,objective,state,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, task.ID, task.Repository, task.Objective, task.State, task.Version, stamp(task.CreatedAt), stamp(task.UpdatedAt)); err != nil {
		return err
	}
	if err = insertEvent(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) Transition(ctx context.Context, taskID string, from, to model.TaskState, event model.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?, version=version+1, updated_at=? WHERE id=? AND state=?`, to, stamp(event.CreatedAt), taskID, from)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("transition %s from %s to %s: state changed concurrently", taskID, from, to)
	}
	event.Sequence, err = nextSequence(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if err := insertEvent(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) AppendEvent(ctx context.Context, event model.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	event.Sequence, err = nextSequence(ctx, tx, event.TaskID)
	if err != nil {
		return err
	}
	if err := insertEvent(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func nextSequence(ctx context.Context, q queryer, taskID string) (int64, error) {
	var seq int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM events WHERE task_id=?`, taskID).Scan(&seq)
	return seq, err
}

func (s *SQLite) RecordRun(ctx context.Context, record model.RunRecord, repository string) error {
	usage, err := json.Marshal(record.Usage)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if record.SessionID != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions(adapter,upstream_id,task_id,repository,healthy,created_at,last_used_at) VALUES(?,?,?,?,1,?,?) ON CONFLICT(adapter,upstream_id) DO UPDATE SET task_id=excluded.task_id,repository=excluded.repository,healthy=1,last_used_at=excluded.last_used_at`, record.Adapter, record.SessionID, record.TaskID, repository, stamp(record.StartedAt), stamp(record.CompletedAt))
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runs(id,task_id,adapter,session_id,status,exit_code,response,usage_json,started_at,completed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, record.ID, record.TaskID, record.Adapter, record.SessionID, record.Status, record.ExitCode, record.Response, string(usage), stamp(record.StartedAt), stamp(record.CompletedAt))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) LatestSession(ctx context.Context, taskID, adapter string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT upstream_id FROM sessions WHERE task_id=? AND adapter=? AND healthy=1 ORDER BY last_used_at DESC LIMIT 1`, taskID, adapter).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertEvent(ctx context.Context, e execer, event model.Event) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return err
	}
	_, err = e.ExecContext(ctx, `INSERT INTO events(id,task_id,sequence,type,actor,summary,data_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, event.ID, event.TaskID, event.Sequence, event.Type, event.Actor, event.Summary, string(data), stamp(event.CreatedAt))
	return err
}

func (s *SQLite) LatestTask(ctx context.Context, repo string) (*model.Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,repository,objective,state,version,created_at,updated_at FROM tasks WHERE repository=? ORDER BY updated_at DESC LIMIT 1`, repo)
	return scanTask(row)
}

func (s *SQLite) Task(ctx context.Context, id string) (*model.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT id,repository,objective,state,version,created_at,updated_at FROM tasks WHERE id=?`, id))
}

type scanner interface{ Scan(...any) error }

func scanTask(row scanner) (*model.Task, error) {
	var t model.Task
	var created, updated string
	if err := row.Scan(&t.ID, &t.Repository, &t.Objective, &t.State, &t.Version, &created, &updated); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &t, nil
}

func (s *SQLite) ListTasks(ctx context.Context, repo string, limit int) ([]model.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,repository,objective,state,version,created_at,updated_at FROM tasks WHERE repository=? ORDER BY updated_at DESC LIMIT ?`, repo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *SQLite) Events(ctx context.Context, taskID string) ([]model.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,sequence,type,actor,summary,data_json,created_at FROM events WHERE task_id=? ORDER BY sequence`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		var raw, created string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Sequence, &e.Type, &e.Actor, &e.Summary, &raw, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &e.Data)
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, e)
	}
	return out, rows.Err()
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
