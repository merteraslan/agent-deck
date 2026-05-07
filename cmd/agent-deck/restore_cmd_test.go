package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

type restoreTestRun struct {
	code         int
	stdout       string
	stderr       string
	saveCalls    int
	restartIDs   []string
	refreshCalls int
	result       restoreResult
}

func runRestoreTest(t *testing.T, args []string, instances []*session.Instance, configure func(*restoreCommandDeps)) restoreTestRun {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	run := restoreTestRun{}

	deps := restoreCommandDeps{
		load: func(profile string) (*session.Storage, []*session.Instance, []*session.GroupData, error) {
			return nil, instances, nil, nil
		},
		save: func(storage *session.Storage, instances []*session.Instance, groups []*session.GroupData) error {
			run.saveCalls++
			return nil
		},
		refreshStatuses: func(instances []*session.Instance) {
			run.refreshCalls++
		},
		restart: func(inst *session.Instance) error {
			run.restartIDs = append(run.restartIDs, inst.ID)
			return nil
		},
		now: func() time.Time {
			return time.Unix(1700000000, 0)
		},
		stdout: &stdout,
		stderr: &stderr,
	}
	if configure != nil {
		configure(&deps)
	}

	run.code = runRestoreCommand("_test", args, deps)
	run.stdout = stdout.String()
	run.stderr = stderr.String()
	if strings.Contains(strings.Join(args, " "), "--json") && run.stdout != "" {
		if err := json.Unmarshal([]byte(run.stdout), &run.result); err != nil {
			t.Fatalf("restore JSON did not unmarshal: %v\n%s", err, run.stdout)
		}
	}
	return run
}

func restoreInst(id string, status session.Status, path string, ts time.Time) *session.Instance {
	return &session.Instance{
		ID:             id,
		Title:          "title-" + id,
		ProjectPath:    path,
		GroupPath:      "my-sessions",
		Command:        "claude",
		Tool:           "claude",
		Status:         status,
		CreatedAt:      ts,
		LastAccessedAt: ts,
	}
}

func TestRestoreDefaultFiltersOnlyErroredSessions(t *testing.T) {
	path := t.TempDir()
	now := time.Unix(1700000000, 0)
	instances := []*session.Instance{
		restoreInst("err", session.StatusError, path, now.Add(-time.Hour)),
		restoreInst("stopped", session.StatusStopped, path, now),
		restoreInst("running", session.StatusRunning, path, now.Add(time.Hour)),
	}

	run := runRestoreTest(t, []string{"--dry-run", "--json"}, instances, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if len(run.result.Restored) != 1 || run.result.Restored[0].ID != "err" {
		t.Fatalf("restored=%+v, want only err", run.result.Restored)
	}
	if run.result.Count != 1 {
		t.Fatalf("count=%d, want 1", run.result.Count)
	}
}

func TestRestoreRefreshesStatusesBeforeSelectingCandidates(t *testing.T) {
	inst := restoreInst("late-error", session.StatusIdle, t.TempDir(), time.Unix(1700000000, 0))

	run := runRestoreTest(t, []string{"--dry-run", "--json"}, []*session.Instance{inst}, func(deps *restoreCommandDeps) {
		deps.refreshStatuses = func(instances []*session.Instance) {
			inst.SetStatusThreadSafe(session.StatusError)
		}
	})

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if len(run.result.Restored) != 1 || run.result.Restored[0].ID != "late-error" {
		t.Fatalf("restored=%+v, want refreshed errored session", run.result.Restored)
	}
}

func TestRestoreIncludeStoppedAndStatusOverridePrecedence(t *testing.T) {
	path := t.TempDir()
	now := time.Unix(1700000000, 0)
	instances := []*session.Instance{
		restoreInst("err", session.StatusError, path, now.Add(-time.Hour)),
		restoreInst("stopped", session.StatusStopped, path, now),
	}
	instances[0].Command = "claude --err"
	instances[1].Command = "claude --stopped"

	includeRun := runRestoreTest(t, []string{"--dry-run", "--json", "--include-stopped"}, instances, nil)
	if includeRun.code != 0 {
		t.Fatalf("include code=%d stderr=%s", includeRun.code, includeRun.stderr)
	}
	if got := idsFromRestoreItems(includeRun.result.Restored); strings.Join(got, ",") != "stopped,err" {
		t.Fatalf("include restored IDs=%v, want stopped,err", got)
	}

	overrideRun := runRestoreTest(t, []string{"--dry-run", "--json", "--include-stopped", "--status", "stopped"}, instances, nil)
	if overrideRun.code != 0 {
		t.Fatalf("override code=%d stderr=%s", overrideRun.code, overrideRun.stderr)
	}
	if got := idsFromRestoreItems(overrideRun.result.Restored); strings.Join(got, ",") != "stopped" {
		t.Fatalf("override restored IDs=%v, want stopped", got)
	}
}

func TestRestoreLastActiveAliasAndRecentLimit(t *testing.T) {
	path := t.TempDir()
	now := time.Unix(1700000000, 0)
	older := restoreInst("older", session.StatusError, path, now.Add(-time.Hour))
	newer := restoreInst("newer", session.StatusError, path, now)
	newer.Command = "codex"

	run := runRestoreTest(t, []string{"--dry-run", "--json", "--last-active", "1"}, []*session.Instance{older, newer}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if len(run.result.Restored) != 1 || run.result.Restored[0].ID != "newer" {
		t.Fatalf("restored=%+v, want only newer", run.result.Restored)
	}
}

func TestRestoreSortsByComputedActivityTimestamp(t *testing.T) {
	path := t.TempDir()
	now := time.Unix(1700000000, 0)
	started := restoreInst("started", session.StatusError, path, now.Add(-3*time.Hour))
	started.Command = "claude --started"
	started.LastStartedAt = now.Add(3 * time.Hour)
	accessed := restoreInst("accessed", session.StatusError, path, now.Add(-2*time.Hour))
	accessed.Command = "claude --accessed"
	accessed.LastAccessedAt = now.Add(2 * time.Hour)
	created := restoreInst("created", session.StatusError, path, now.Add(time.Hour))
	created.Command = "claude --created"
	created.LastAccessedAt = time.Time{}

	run := runRestoreTest(t, []string{"--dry-run", "--json"}, []*session.Instance{created, accessed, started}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if got := strings.Join(idsFromRestoreItems(run.result.Restored), ","); got != "started,accessed,created" {
		t.Fatalf("restore order=%s, want started,accessed,created", got)
	}
}

func TestRestoreInvalidArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "recent zero", args: []string{"--recent", "0"}},
		{name: "last active zero", args: []string{"--last-active", "0"}},
		{name: "recent and last active", args: []string{"--recent", "2", "--last-active", "1"}},
		{name: "unknown status", args: []string{"--status", "error,unknown"}},
		{name: "positional", args: []string{"some-session"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loadCalled := false
			run := runRestoreTest(t, tt.args, nil, func(deps *restoreCommandDeps) {
				deps.load = func(profile string) (*session.Storage, []*session.Instance, []*session.GroupData, error) {
					loadCalled = true
					return nil, nil, nil, nil
				}
			})
			if run.code != 2 {
				t.Fatalf("code=%d, want 2; stderr=%s", run.code, run.stderr)
			}
			if loadCalled {
				t.Fatal("load should not be called for invalid args")
			}
		})
	}
}

func TestRestoreRejectsLiveStatuses(t *testing.T) {
	for _, status := range []string{"running", "waiting", "idle", "starting"} {
		t.Run(status, func(t *testing.T) {
			run := runRestoreTest(t, []string{"--status", status}, nil, nil)
			if run.code != 2 {
				t.Fatalf("code=%d, want 2; stderr=%s", run.code, run.stderr)
			}
			if !strings.Contains(run.stderr, "live status") {
				t.Fatalf("stderr=%q, want live status error", run.stderr)
			}
		})
	}
}

func TestRestoreDryRunDoesNotRestartStampOrSave(t *testing.T) {
	inst := restoreInst("err", session.StatusError, t.TempDir(), time.Unix(1700000000, 0))

	run := runRestoreTest(t, []string{"--dry-run"}, []*session.Instance{inst}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if len(run.restartIDs) != 0 {
		t.Fatalf("restart IDs=%v, want none", run.restartIDs)
	}
	if run.saveCalls != 0 {
		t.Fatalf("saveCalls=%d, want 0", run.saveCalls)
	}
	if !inst.LastStartedAt.IsZero() {
		t.Fatalf("LastStartedAt=%v, want zero", inst.LastStartedAt)
	}
}

func TestRestoreJSONShape(t *testing.T) {
	path := t.TempDir()
	inst := restoreInst("err", session.StatusError, path, time.Unix(1700000000, 0))
	inst.WorktreeBranch = "feature/one"

	run := runRestoreTest(t, []string{"--dry-run", "--json"}, []*session.Instance{inst}, nil)
	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(run.stdout), &raw); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	for _, key := range []string{"restored", "skipped", "failed", "dry_run", "count"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("JSON key %q missing from %v", key, raw)
		}
	}
	items := raw["restored"].([]any)
	if len(items) != 1 {
		t.Fatalf("restored len=%d, want 1", len(items))
	}
	item := items[0].(map[string]any)
	for _, key := range []string{"id", "title", "tool", "status", "path", "worktree_branch"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("restored item key %q missing from %v", key, item)
		}
	}
}

func TestRestoreQuietStillPrintsFailuresToStderr(t *testing.T) {
	inst := restoreInst("err", session.StatusError, t.TempDir(), time.Unix(1700000000, 0))
	restartErr := errors.New("boom")

	run := runRestoreTest(t, []string{"--quiet"}, []*session.Instance{inst}, func(deps *restoreCommandDeps) {
		deps.restart = func(inst *session.Instance) error {
			return restartErr
		}
	})

	if run.code != 1 {
		t.Fatalf("code=%d, want 1", run.code)
	}
	if run.stdout != "" {
		t.Fatalf("stdout=%q, want empty", run.stdout)
	}
	if !strings.Contains(run.stderr, "boom") {
		t.Fatalf("stderr=%q, want failure", run.stderr)
	}
}

func TestRestoreNoCandidates(t *testing.T) {
	inst := restoreInst("idle", session.StatusIdle, t.TempDir(), time.Unix(1700000000, 0))

	run := runRestoreTest(t, []string{"--json"}, []*session.Instance{inst}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if run.result.Count != 0 {
		t.Fatalf("count=%d, want 0", run.result.Count)
	}
	if run.saveCalls != 0 || len(run.restartIDs) != 0 {
		t.Fatalf("saveCalls=%d restartIDs=%v, want none", run.saveCalls, run.restartIDs)
	}
}

func TestRestoreFailureExitsOneWithoutSaving(t *testing.T) {
	inst := restoreInst("err", session.StatusError, t.TempDir(), time.Unix(1700000000, 0))

	run := runRestoreTest(t, nil, []*session.Instance{inst}, func(deps *restoreCommandDeps) {
		deps.restart = func(inst *session.Instance) error {
			return errors.New("restart failed")
		}
	})

	if run.code != 1 {
		t.Fatalf("code=%d, want 1", run.code)
	}
	if run.saveCalls != 0 {
		t.Fatalf("saveCalls=%d, want 0", run.saveCalls)
	}
	if !inst.LastStartedAt.IsZero() {
		t.Fatalf("failed restore stamped LastStartedAt=%v", inst.LastStartedAt)
	}
}

func TestRestorePartialSuccessPersistsBeforeExitOne(t *testing.T) {
	path := t.TempDir()
	now := time.Unix(1700000000, 0)
	success := restoreInst("success", session.StatusError, path, now)
	success.Command = "claude --success"
	failed := restoreInst("failed", session.StatusError, path, now.Add(-time.Minute))
	failed.Command = "claude --failed"
	stamp := time.Unix(1800000000, 0)

	run := runRestoreTest(t, nil, []*session.Instance{success, failed}, func(deps *restoreCommandDeps) {
		deps.now = func() time.Time { return stamp }
		deps.restart = func(inst *session.Instance) error {
			if inst.ID == "failed" {
				return errors.New("restart failed")
			}
			return nil
		}
	})

	if run.code != 1 {
		t.Fatalf("code=%d, want 1", run.code)
	}
	if run.saveCalls != 1 {
		t.Fatalf("saveCalls=%d, want 1", run.saveCalls)
	}
	if !success.LastStartedAt.Equal(stamp) {
		t.Fatalf("success LastStartedAt=%v, want %v", success.LastStartedAt, stamp)
	}
	if !failed.LastStartedAt.IsZero() {
		t.Fatalf("failed LastStartedAt=%v, want zero", failed.LastStartedAt)
	}
}

func TestRestoreMissingPathIsSkipped(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	inst := restoreInst("missing", session.StatusError, missing, time.Unix(1700000000, 0))

	run := runRestoreTest(t, []string{"--json"}, []*session.Instance{inst}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if len(run.result.Skipped) != 1 {
		t.Fatalf("skipped=%+v, want one", run.result.Skipped)
	}
	if run.result.Skipped[0].Reason != "path missing" {
		t.Fatalf("reason=%q, want path missing", run.result.Skipped[0].Reason)
	}
	if len(run.restartIDs) != 0 || run.saveCalls != 0 {
		t.Fatalf("restartIDs=%v saveCalls=%d, want none", run.restartIDs, run.saveCalls)
	}
}

func TestRestoreUsesWorktreePathBeforeProjectPath(t *testing.T) {
	projectPath := t.TempDir()
	missingWorktree := filepath.Join(t.TempDir(), "missing-worktree")
	inst := restoreInst("wt", session.StatusError, projectPath, time.Unix(1700000000, 0))
	inst.WorktreePath = missingWorktree
	inst.WorktreeBranch = "feature/wt"

	run := runRestoreTest(t, []string{"--json"}, []*session.Instance{inst}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if len(run.result.Skipped) != 1 {
		t.Fatalf("skipped=%+v, want one", run.result.Skipped)
	}
	if run.result.Skipped[0].Path != missingWorktree {
		t.Fatalf("path=%q, want worktree path %q", run.result.Skipped[0].Path, missingWorktree)
	}
}

func TestRestoreCustomToolDedupeKeepsNewestPerToolAndConfig(t *testing.T) {
	path := t.TempDir()
	now := time.Unix(1700000000, 0)
	oldCustom := restoreInst("old-custom", session.StatusError, path, now.Add(-time.Hour))
	oldCustom.Tool = "my-claude"
	oldCustom.Command = "claude"
	oldCustom.Wrapper = "wrap {command}"
	newCustom := restoreInst("new-custom", session.StatusError, path, now)
	newCustom.Tool = "my-claude"
	newCustom.Command = oldCustom.Command
	newCustom.Wrapper = oldCustom.Wrapper
	otherTool := restoreInst("other-tool", session.StatusError, path, now.Add(-time.Minute))
	otherTool.Tool = "other-claude"
	otherTool.Command = oldCustom.Command
	otherTool.Wrapper = oldCustom.Wrapper

	run := runRestoreTest(t, []string{"--dry-run", "--json"}, []*session.Instance{oldCustom, newCustom, otherTool}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	got := idsFromRestoreItems(run.result.Restored)
	if strings.Join(got, ",") != "new-custom,other-tool" {
		t.Fatalf("restored IDs=%v, want new-custom,other-tool", got)
	}
}

func TestRestoreDedupeKeepsSamePathCodexSessionsWithDifferentSessionIDs(t *testing.T) {
	path := t.TempDir()
	now := time.Unix(1700000000, 0)
	cm := restoreInst("cm", session.StatusError, path, now.Add(-2*time.Minute))
	cm.Tool = "codex"
	cm.Command = "codex"
	cm.CodexSessionID = "codex-cm"
	backend := restoreInst("backend", session.StatusError, path, now.Add(-time.Minute))
	backend.Tool = "codex"
	backend.Command = "codex"
	backend.CodexSessionID = "codex-backend"
	render := restoreInst("render", session.StatusError, path, now)
	render.Tool = "codex"
	render.Command = "codex"
	render.CodexSessionID = "codex-render"

	run := runRestoreTest(t, []string{"--dry-run", "--json"}, []*session.Instance{cm, backend, render}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if got := strings.Join(idsFromRestoreItems(run.result.Restored), ","); got != "render,backend,cm" {
		t.Fatalf("restored IDs=%v, want render,backend,cm", idsFromRestoreItems(run.result.Restored))
	}
}

func TestRestoreDedupeKeepsNewestSamePathCodexSessionWithSameSessionID(t *testing.T) {
	path := t.TempDir()
	now := time.Unix(1700000000, 0)
	old := restoreInst("old-codex", session.StatusError, path, now.Add(-time.Hour))
	old.Tool = "codex"
	old.Command = "codex"
	old.CodexSessionID = "same-codex-session"
	newer := restoreInst("new-codex", session.StatusError, path, now)
	newer.Tool = "codex"
	newer.Command = "codex"
	newer.CodexSessionID = old.CodexSessionID

	run := runRestoreTest(t, []string{"--dry-run", "--json"}, []*session.Instance{old, newer}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if got := strings.Join(idsFromRestoreItems(run.result.Restored), ","); got != "new-codex" {
		t.Fatalf("restored IDs=%v, want new-codex", idsFromRestoreItems(run.result.Restored))
	}
}

func TestRestoreDedupeFallsBackWhenCodexSessionIDMissing(t *testing.T) {
	path := t.TempDir()
	now := time.Unix(1700000000, 0)
	old := restoreInst("old-codex", session.StatusError, path, now.Add(-time.Hour))
	old.Tool = "codex"
	old.Command = "codex"
	newer := restoreInst("new-codex", session.StatusError, path, now)
	newer.Tool = "codex"
	newer.Command = "codex"

	run := runRestoreTest(t, []string{"--dry-run", "--json"}, []*session.Instance{old, newer}, nil)

	if run.code != 0 {
		t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
	}
	if got := strings.Join(idsFromRestoreItems(run.result.Restored), ","); got != "new-codex" {
		t.Fatalf("restored IDs=%v, want new-codex", idsFromRestoreItems(run.result.Restored))
	}
}

func TestRestoreDedupeUsesToolSessionIDsForBuiltInTools(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		setID   func(*session.Instance, string)
		wantIDs string
	}{
		{
			name: "claude",
			tool: "claude",
			setID: func(inst *session.Instance, id string) {
				inst.ClaudeSessionID = id
			},
			wantIDs: "new-b,new-a",
		},
		{
			name: "gemini",
			tool: "gemini",
			setID: func(inst *session.Instance, id string) {
				inst.GeminiSessionID = id
			},
			wantIDs: "new-b,new-a",
		},
		{
			name: "opencode",
			tool: "opencode",
			setID: func(inst *session.Instance, id string) {
				inst.OpenCodeSessionID = id
			},
			wantIDs: "new-b,new-a",
		},
		{
			name: "codex",
			tool: "codex",
			setID: func(inst *session.Instance, id string) {
				inst.CodexSessionID = id
			},
			wantIDs: "new-b,new-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := t.TempDir()
			now := time.Unix(1700000000, 0)
			oldA := restoreInst("old-a", session.StatusError, path, now.Add(-3*time.Hour))
			oldA.Tool = tt.tool
			oldA.Command = tt.tool
			tt.setID(oldA, "session-a")
			newA := restoreInst("new-a", session.StatusError, path, now.Add(-time.Hour))
			newA.Tool = tt.tool
			newA.Command = tt.tool
			tt.setID(newA, "session-a")
			newB := restoreInst("new-b", session.StatusError, path, now)
			newB.Tool = tt.tool
			newB.Command = tt.tool
			tt.setID(newB, "session-b")

			run := runRestoreTest(t, []string{"--dry-run", "--json"}, []*session.Instance{oldA, newA, newB}, nil)

			if run.code != 0 {
				t.Fatalf("code=%d stderr=%s", run.code, run.stderr)
			}
			if got := strings.Join(idsFromRestoreItems(run.result.Restored), ","); got != tt.wantIDs {
				t.Fatalf("restored IDs=%v, want %s", idsFromRestoreItems(run.result.Restored), tt.wantIDs)
			}
		})
	}
}

func TestRestoreLoadFailureExitsTwo(t *testing.T) {
	run := runRestoreTest(t, nil, nil, func(deps *restoreCommandDeps) {
		deps.load = func(profile string) (*session.Storage, []*session.Instance, []*session.GroupData, error) {
			return nil, nil, nil, errors.New("db load failed")
		}
	})

	if run.code != 2 {
		t.Fatalf("code=%d, want 2", run.code)
	}
	if !strings.Contains(run.stderr, "db load failed") {
		t.Fatalf("stderr=%q, want load failure", run.stderr)
	}
}

func idsFromRestoreItems(items []restoreOutputItem) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}
