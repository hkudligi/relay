package core

import "testing"

func TestParseDelegatedTasks(t *testing.T) {
	response := `plan
<rly-subagents>
[{"id":"process","agent":"codex","prompt":"edit process","workspace_paths":["internal/agents/process.go"],"parallel_safe":true},{"id":"store","agent":"agy","prompt":"edit store","workspace_paths":["internal/store/sqlite.go"],"parallel_safe":true}]
</rly-subagents>`
	tasks, err := ParseDelegatedTasks(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].ID != "process" || tasks[1].Agent != "agy" {
		t.Fatalf("tasks = %+v", tasks)
	}
}

func TestParseDelegatedTasksRequiresOwnership(t *testing.T) {
	_, err := ParseDelegatedTasks(`<rly-subagents>[{"id":"x","agent":"codex","prompt":"edit"}]</rly-subagents>`)
	if err == nil {
		t.Fatal("expected ownership validation error")
	}
}
