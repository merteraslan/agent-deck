package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

type restoreOptions struct {
	dryRun bool
	json   bool
	quiet  bool
	recent int
	status map[session.Status]struct{}
}

type restoreOutputItem struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Tool           string `json:"tool"`
	Status         string `json:"status"`
	Path           string `json:"path"`
	WorktreeBranch string `json:"worktree_branch"`
	Reason         string `json:"reason,omitempty"`
	Error          string `json:"error,omitempty"`
}

type restoreResult struct {
	Restored []restoreOutputItem `json:"restored"`
	Skipped  []restoreOutputItem `json:"skipped"`
	Failed   []restoreOutputItem `json:"failed"`
	DryRun   bool                `json:"dry_run"`
	Count    int                 `json:"count"`
}

type restoreCommandDeps struct {
	load            func(profile string) (*session.Storage, []*session.Instance, []*session.GroupData, error)
	save            func(storage *session.Storage, instances []*session.Instance, groups []*session.GroupData) error
	refreshStatuses func(instances []*session.Instance)
	restart         func(inst *session.Instance) error
	now             func() time.Time
	stat            func(path string) (os.FileInfo, error)
	stdout          io.Writer
	stderr          io.Writer
}

func defaultRestoreDeps() restoreCommandDeps {
	return restoreCommandDeps{
		load:            loadSessionData,
		save:            saveSessionData,
		refreshStatuses: refreshRestoreStatusesReadOnly,
		restart: func(inst *session.Instance) error {
			return inst.Restart()
		},
		now:    time.Now,
		stat:   os.Stat,
		stdout: os.Stdout,
		stderr: os.Stderr,
	}
}

func handleRestore(profile string, args []string) {
	code := runRestoreCommand(profile, args, defaultRestoreDeps())
	if code != 0 {
		os.Exit(code)
	}
}

func runRestoreCommand(profile string, args []string, deps restoreCommandDeps) int {
	deps = normalizeRestoreDeps(deps)
	opts, code, ok := parseRestoreOptions(args, deps.stderr)
	if !ok {
		return code
	}

	storage, instances, groups, err := deps.load(profile)
	if err != nil {
		fmt.Fprintf(deps.stderr, "Error: failed to load sessions: %v\n", err)
		return 2
	}

	deps.refreshStatuses(instances)
	candidates := selectRestoreCandidates(instances, opts.status, opts.recent)

	result := restoreResult{
		Restored: []restoreOutputItem{},
		Skipped:  []restoreOutputItem{},
		Failed:   []restoreOutputItem{},
		DryRun:   opts.dryRun,
	}

	successCount := 0
	for _, inst := range candidates {
		item := restoreItemFromInstance(inst)
		if restorePathMissing(item.Path, deps.stat) {
			item.Reason = "path missing"
			result.Skipped = append(result.Skipped, item)
			continue
		}

		if opts.dryRun {
			result.Restored = append(result.Restored, item)
			continue
		}

		if err := deps.restart(inst); err != nil {
			item.Error = err.Error()
			result.Failed = append(result.Failed, item)
			fmt.Fprintf(deps.stderr, "Error: failed to restore session %q: %v\n", inst.Title, err)
			continue
		}

		inst.LastStartedAt = deps.now()
		result.Restored = append(result.Restored, item)
		successCount++
	}

	if !opts.dryRun && successCount > 0 {
		if err := deps.save(storage, instances, groups); err != nil {
			item := restoreOutputItem{Error: fmt.Sprintf("failed to save session state: %v", err)}
			result.Failed = append(result.Failed, item)
			fmt.Fprintf(deps.stderr, "Error: failed to save session state: %v\n", err)
		}
	}

	result.Count = len(result.Restored) + len(result.Skipped) + len(result.Failed)
	writeRestoreOutput(deps.stdout, opts, result)

	if len(result.Failed) > 0 {
		return 1
	}
	return 0
}

func normalizeRestoreDeps(deps restoreCommandDeps) restoreCommandDeps {
	defaults := defaultRestoreDeps()
	if deps.load == nil {
		deps.load = defaults.load
	}
	if deps.save == nil {
		deps.save = defaults.save
	}
	if deps.refreshStatuses == nil {
		deps.refreshStatuses = defaults.refreshStatuses
	}
	if deps.restart == nil {
		deps.restart = defaults.restart
	}
	if deps.now == nil {
		deps.now = defaults.now
	}
	if deps.stat == nil {
		deps.stat = defaults.stat
	}
	if deps.stdout == nil {
		deps.stdout = defaults.stdout
	}
	if deps.stderr == nil {
		deps.stderr = defaults.stderr
	}
	return deps
}

func parseRestoreOptions(args []string, stderr io.Writer) (restoreOptions, int, bool) {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dryRun := fs.Bool("dry-run", false, "Show sessions that would be restored without restarting them")
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	quiet := fs.Bool("quiet", false, "Suppress success chatter")
	quietShort := fs.Bool("q", false, "Suppress success chatter (short)")
	includeStopped := fs.Bool("include-stopped", false, "Include stopped sessions in addition to errored sessions")
	recent := fs.Int("recent", 10, "Number of recent matching sessions to restore")
	lastActive := fs.Int("last-active", 0, "Alias for --recent")
	statusCSV := fs.String("status", "", "Comma-separated statuses to restore: error,stopped")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: agent-deck restore [options]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Restart recent errored sessions from the existing registry.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Options:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return restoreOptions{}, 0, false
		}
		return restoreOptions{}, 2, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "Error: restore does not accept positional arguments: %s\n", strings.Join(fs.Args(), " "))
		return restoreOptions{}, 2, false
	}

	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		seen[f.Name] = true
	})
	if seen["recent"] && seen["last-active"] {
		fmt.Fprintln(stderr, "Error: --recent and --last-active cannot be used together")
		return restoreOptions{}, 2, false
	}
	limit := *recent
	if seen["last-active"] {
		limit = *lastActive
	}
	if limit <= 0 {
		if seen["last-active"] {
			fmt.Fprintln(stderr, "Error: --last-active must be greater than 0")
		} else {
			fmt.Fprintln(stderr, "Error: --recent must be greater than 0")
		}
		return restoreOptions{}, 2, false
	}

	statusFilter, err := parseRestoreStatusFilter(*statusCSV, *includeStopped, seen["status"])
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return restoreOptions{}, 2, false
	}

	return restoreOptions{
		dryRun: *dryRun,
		json:   *jsonOutput,
		quiet:  *quiet || *quietShort,
		recent: limit,
		status: statusFilter,
	}, 0, true
}

func parseRestoreStatusFilter(raw string, includeStopped bool, explicit bool) (map[session.Status]struct{}, error) {
	filter := map[session.Status]struct{}{}
	if strings.TrimSpace(raw) == "" {
		if explicit {
			return nil, fmt.Errorf("--status requires at least one status")
		}
		filter[session.StatusError] = struct{}{}
		if includeStopped {
			filter[session.StatusStopped] = struct{}{}
		}
		return filter, nil
	}

	for _, part := range strings.Split(raw, ",") {
		status := strings.ToLower(strings.TrimSpace(part))
		switch session.Status(status) {
		case session.StatusError:
			filter[session.StatusError] = struct{}{}
		case session.StatusStopped:
			filter[session.StatusStopped] = struct{}{}
		case session.StatusRunning, session.StatusWaiting, session.StatusIdle, session.StatusStarting:
			return nil, fmt.Errorf("live status %q cannot be restored", status)
		default:
			if status == "" {
				return nil, fmt.Errorf("--status contains an empty status")
			}
			return nil, fmt.Errorf("unsupported restore status %q (supported: error,stopped)", status)
		}
	}
	return filter, nil
}

func refreshRestoreStatusesReadOnly(instances []*session.Instance) {
	session.RefreshInstancesForCLIStatus(instances)
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		inst.ForceNextStatusCheck()
		_ = inst.UpdateStatus()
	}
}

func selectRestoreCandidates(instances []*session.Instance, statuses map[session.Status]struct{}, recent int) []*session.Instance {
	candidates := make([]*session.Instance, 0, len(instances))
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		if _, ok := statuses[inst.GetStatusThreadSafe()]; ok {
			candidates = append(candidates, inst)
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		ti := restoreSortTimestamp(candidates[i])
		tj := restoreSortTimestamp(candidates[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return candidates[i].ID < candidates[j].ID
	})

	deduped := make([]*session.Instance, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, inst := range candidates {
		key := restoreDedupKey(inst)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, inst)
		if len(deduped) == recent {
			break
		}
	}

	return deduped
}

func restoreSortTimestamp(inst *session.Instance) time.Time {
	if inst == nil {
		return time.Time{}
	}
	if !inst.LastStartedAt.IsZero() {
		return inst.LastStartedAt
	}
	if !inst.LastAccessedAt.IsZero() {
		return inst.LastAccessedAt
	}
	return inst.CreatedAt
}

func restoreDedupKey(inst *session.Instance) string {
	path := effectiveRestorePath(inst)
	if path != "" {
		path = filepath.Clean(path)
	}
	parts := []string{
		inst.Tool,
		path,
		inst.WorktreeBranch,
		inst.Command,
		inst.Wrapper,
	}
	if toolSessionID := restoreToolSessionID(inst); toolSessionID != "" {
		parts = append(parts, "session_id", toolSessionID)
	}
	return strings.Join(parts, "\x00")
}

func restoreToolSessionID(inst *session.Instance) string {
	if inst == nil {
		return ""
	}
	switch {
	case session.IsCodexCompatible(inst.Tool):
		return strings.TrimSpace(inst.CodexSessionID)
	case session.IsClaudeCompatible(inst.Tool):
		return strings.TrimSpace(inst.ClaudeSessionID)
	case inst.Tool == "gemini":
		return strings.TrimSpace(inst.GeminiSessionID)
	case inst.Tool == "opencode":
		return strings.TrimSpace(inst.OpenCodeSessionID)
	default:
		return ""
	}
}

func effectiveRestorePath(inst *session.Instance) string {
	if inst == nil {
		return ""
	}
	if strings.TrimSpace(inst.WorktreePath) != "" {
		return inst.WorktreePath
	}
	return inst.ProjectPath
}

func restoreItemFromInstance(inst *session.Instance) restoreOutputItem {
	if inst == nil {
		return restoreOutputItem{}
	}
	return restoreOutputItem{
		ID:             inst.ID,
		Title:          inst.Title,
		Tool:           inst.Tool,
		Status:         string(inst.GetStatusThreadSafe()),
		Path:           effectiveRestorePath(inst),
		WorktreeBranch: inst.WorktreeBranch,
	}
}

func restorePathMissing(path string, stat func(string) (os.FileInfo, error)) bool {
	if strings.TrimSpace(path) == "" {
		return true
	}
	info, err := stat(path)
	if err != nil {
		return true
	}
	return !info.IsDir()
}

func writeRestoreOutput(w io.Writer, opts restoreOptions, result restoreResult) {
	if opts.json {
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			fmt.Fprintf(w, `{"restored":[],"skipped":[],"failed":[{"error":"failed to format JSON: %s"}],"dry_run":%t,"count":1}`+"\n", err.Error(), opts.dryRun)
			return
		}
		fmt.Fprintln(w, string(data))
		return
	}
	if opts.quiet {
		return
	}

	restored := len(result.Restored)
	skipped := len(result.Skipped)
	failed := len(result.Failed)
	switch {
	case result.DryRun:
		fmt.Fprintf(w, "Would restore %d session(s)", restored)
	case result.Count == 0:
		fmt.Fprintln(w, "No matching sessions to restore.")
		return
	default:
		fmt.Fprintf(w, "Restored %d session(s)", restored)
	}
	if skipped > 0 {
		fmt.Fprintf(w, ", skipped %d", skipped)
	}
	if failed > 0 {
		fmt.Fprintf(w, ", failed %d", failed)
	}
	fmt.Fprintln(w)
}
