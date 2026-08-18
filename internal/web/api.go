package web

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jnk/deadlocker/internal/agentapi"
	"github.com/jnk/deadlocker/internal/casedef"
	"github.com/jnk/deadlocker/internal/chat"
	"github.com/jnk/deadlocker/internal/engine"
	"github.com/jnk/deadlocker/internal/store"
)

// ------------------------------------------------------------ activity feed

// handleActivity streams the global activity feed. It is what makes a change
// performed by an MCP client or by the assistant show up immediately in an
// open browser tab.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	since := 0
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		fmt.Sscanf(v, "%d", &since)
	} else if v := r.URL.Query().Get("since"); v != "" {
		fmt.Sscanf(v, "%d", &since)
	}

	ch, backlog, cancel := s.api.Hub().Subscribe(since)
	defer cancel()

	send := func(a agentapi.Activity) bool {
		payload, err := json.Marshal(a)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: activity\ndata: %s\n\n", a.Seq, payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, a := range backlog {
		if !send(a) {
			return
		}
	}
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case a, open := <-ch:
			if !open {
				return
			}
			if !send(a) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-s.quit:
			return
		}
	}
}

// ---------------------------------------------------------------- settings

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	pd := s.base("Settings", "settings")
	cfg, version, err := s.store.Current()
	if err != nil {
		pd.Error = err.Error()
	}
	pd.Config = &cfg
	pd.ConfigVersion = version
	pd.HasAPIKey = cfg.LLM.APIKey != ""
	// The key itself never reaches the browser.
	pd.Config.LLM.APIKey = ""
	if versions, err := s.store.Versions(25); err == nil {
		for _, v := range versions {
			pd.ConfigVersions = append(pd.ConfigVersions, v.Redacted())
		}
	}
	pd.StorePath = s.store.Path()
	pd.MCPURL = "http://" + r.Host + "/mcp"
	s.render(w, "settings.html", pd)
}

type settingsPayload struct {
	Enabled     bool   `json:"enabled"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key"`
	ClearAPIKey bool   `json:"clear_api_key"`
	Model       string `json:"model"`
	// Sampling settings are pointers: absent means "leave it to the model
	// server" rather than "use zero".
	Temperature      *float64 `json:"temperature"`
	MaxTokens        *int     `json:"max_tokens"`
	TopP             *float64 `json:"top_p"`
	TopK             *int64   `json:"top_k"`
	MinP             *float64 `json:"min_p"`
	RepeatPenalty    *float64 `json:"repeat_penalty"`
	PresencePenalty  *float64 `json:"presence_penalty"`
	FrequencyPenalty *float64 `json:"frequency_penalty"`
	Seed             *int64   `json:"seed"`
	Effort           string   `json:"effort"`
	Extra            string   `json:"extra"`
	MaxSteps         *int     `json:"max_steps"`

	// Container settings live in the same form; they are one checkbox and a
	// field, and a second Save button would be worse than mixing them.
	Prewarm      bool   `json:"prewarm"`
	PrewarmImage string `json:"prewarm_image"`

	Note string `json:"note"`
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	var req settingsPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	current, _, err := s.store.Current()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	next := current
	next.LLM.Enabled = req.Enabled
	next.LLM.BaseURL = req.BaseURL
	next.LLM.Model = req.Model
	next.LLM.Temperature = req.Temperature
	next.LLM.MaxTokens = req.MaxTokens
	next.LLM.TopP = req.TopP
	next.LLM.TopK = req.TopK
	next.LLM.MinP = req.MinP
	next.LLM.RepeatPenalty = req.RepeatPenalty
	next.LLM.PresencePenalty = req.PresencePenalty
	next.LLM.FrequencyPenalty = req.FrequencyPenalty
	next.LLM.Seed = req.Seed
	next.LLM.Effort = strings.TrimSpace(req.Effort)
	next.LLM.Extra = strings.TrimSpace(req.Extra)
	next.LLM.MaxSteps = req.MaxSteps
	next.MySQL.Prewarm = req.Prewarm
	next.MySQL.PrewarmImage = strings.TrimSpace(req.PrewarmImage)

	// The kwargs object is rejected here rather than at request time, so a typo
	// surfaces while the settings page is still open instead of as a failed
	// chat later.
	if _, err := next.LLM.ExtraParams(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// The browser never receives the stored key, so an empty field means
	// "leave it alone". Clearing is an explicit action.
	switch {
	case req.ClearAPIKey:
		next.LLM.APIKey = ""
	case req.APIKey != "":
		next.LLM.APIKey = req.APIKey
	}

	saved, err := s.store.Save(next, req.Note)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.api.Hub().Publish(agentapi.Activity{
		Source: agentapi.SourceUI, Kind: agentapi.KindConfigSaved,
		Summary: fmt.Sprintf("saved configuration version %d", saved.Version),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": saved.Version, "ready": next.Ready(),
	})
}

func (s *Server) handleSettingsRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version uint64 `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	saved, err := s.store.Restore(req.Version)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": saved.Version})
}

// handleCancelJob stops a running analysis.
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	out, err := s.api.CancelJob(r.Context(), agentapi.CancelJobInput{JobID: r.PathValue("id")})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "cancelled": out.Cancelled, "note": out.Note,
	})
}

// ------------------------------------------------------------- run log

// handleForgetRun removes one run from the log. A run still open is closed
// first: taking it out of the log while it holds a scratch database and a set of
// connections would leak both.
func (s *Server) handleForgetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, live := s.mgr.Get(id); live {
		if err := s.mgr.CloseRun(id); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	s.mgr.History().Forget(id)
	// Broadcast, so a second browser tab -- or a colleague looking at the same
	// server -- does not keep offering a run that is no longer there.
	s.api.Hub().Publish(agentapi.Activity{
		Source: agentapi.SourceUI, Kind: agentapi.KindRunForgotten, RunID: id,
		Summary: "removed run " + id + " from the log",
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleClearRuns empties the log. Runs that are still open are left alone:
// clearing a list is a tidying action, not a request to tear down work in
// progress.
func (s *Server) handleClearRuns(w http.ResponseWriter, r *http.Request) {
	live := map[string]bool{}
	for _, run := range s.mgr.List() {
		live[run.State().ID] = true
	}
	n := 0
	for _, rec := range s.mgr.History().All() {
		if live[rec.RunID] {
			continue
		}
		if s.mgr.History().Forget(rec.RunID) {
			n++
		}
	}
	s.api.Hub().Publish(agentapi.Activity{
		Source: agentapi.SourceUI, Kind: agentapi.KindRunForgotten,
		Summary: fmt.Sprintf("cleared %d run(s) from the log", n),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n, "kept": len(live)})
}

// handleSeedBuiltIns writes the scenarios that ship with the binary into the
// case directory.
//
// This is a deliberate action rather than something that happens on first
// start: two dozen files appearing in a directory nobody asked to have filled
// is a decision the user should make. Existing files are never overwritten, so
// running it twice is safe and the second time reports nothing written.
func (s *Server) handleSeedBuiltIns(w http.ResponseWriter, r *http.Request) {
	res, err := casedef.Seed(s.lib.Root)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := s.lib.Load(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if len(res.Written) > 0 {
		s.api.Hub().Publish(agentapi.Activity{
			Source: agentapi.SourceUI, Kind: agentapi.KindScenarioCreated,
			Summary: fmt.Sprintf("imported %d built-in scenario(s)", len(res.Written)),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "written": len(res.Written), "skipped": res.Skipped,
	})
}

// handleRemoveBuiltIns deletes the shipped scenarios again, sparing any that
// have been edited.
func (s *Server) handleRemoveBuiltIns(w http.ResponseWriter, r *http.Request) {
	removed, kept, err := casedef.RemoveBuiltIns(s.lib.Root)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := s.lib.Load(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if removed > 0 {
		s.api.Hub().Publish(agentapi.Activity{
			Source: agentapi.SourceUI, Kind: agentapi.KindScenarioUpdated,
			Summary: fmt.Sprintf("removed %d built-in scenario(s)", removed),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "removed": removed, "kept": kept,
	})
}

// handleBuiltInCount reports how many built-in scenarios exist and how many are
// already on disk, so the offer to import them can say what it will do.
func (s *Server) handleBuiltInCount(w http.ResponseWriter, r *http.Request) {
	total, present := casedef.BuiltInStatus(s.lib.Root)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": total, "present": present, "missing": total - present,
	})
}

// -------------------------------------------------- scenario import/export

// bundleFormat identifies a Deadlocker scenario bundle. A shared file should
// say what it is, and refusing to import something that is not one is friendlier
// than failing on a missing field later.
const (
	bundleFormat  = "deadlocker.scenario"
	bundleVersion = 1
)

// scenarioBundle is a scenario packaged for sharing.
//
// The YAML alone is enough to reproduce a scenario, so that is the whole of the
// required payload. The optional run history is what makes a bug report useful:
// "here is the scenario, and here is what it did on my machine".
type scenarioBundle struct {
	Format   string                  `json:"format"`
	Version  int                     `json:"version"`
	Exported time.Time               `json:"exported_at"`
	ID       string                  `json:"id"`
	Name     string                  `json:"name"`
	Path     string                  `json:"path,omitempty"`
	YAML     string                  `json:"yaml"`
	Runs     []*engine.Record        `json:"runs,omitempty"`
	Versions []store.ScenarioVersion `json:"versions,omitempty"`
}

// handleExportScenario writes a scenario as a shareable file. Without
// ?history=1 it is the YAML itself, which is the most useful thing to hand
// someone; with it, a JSON bundle carrying the runs too.
func (s *Server) handleExportScenario(w http.ResponseWriter, r *http.Request) {
	c, ok := s.lib.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown scenario"})
		return
	}

	withHistory := r.URL.Query().Get("history") == "1"
	withVersions := r.URL.Query().Get("versions") == "1"
	if !withHistory && !withVersions {
		w.Header().Set("Content-Type", "application/x-yaml; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%s.yaml", c.ID))
		_, _ = io.WriteString(w, c.Source)
		return
	}

	bundle := scenarioBundle{
		Format: bundleFormat, Version: bundleVersion, Exported: time.Now(),
		ID: c.ID, Name: c.Name, Path: c.Path, YAML: c.Source,
	}
	if withHistory {
		bundle.Runs = s.mgr.History().ForCase(c.ID)
	}
	if withVersions {
		bundle.Versions, _ = s.store.ScenarioVersions(c.ID, c.Source, 0)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%s.deadlocker.json", c.ID))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(bundle)
}

// handleInspectImport reports what a file contains without writing anything.
//
// Dropping a file is a one-gesture action with no undo, and a bundle can carry
// a scenario plus dozens of runs and versions. Saying what is in it — and what
// it will be called — before anything touches disk is the difference between a
// gesture and a decision.
func (s *Server) handleInspectImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	kind := "scenario"
	var bundle scenarioBundle
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		kind = "bundle"
	}

	yamlSrc, err := unwrapImport(body)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if kind == "bundle" {
		_ = json.Unmarshal([]byte(trimmed), &bundle)
	}

	parsed, err := casedef.Parse([]byte(yamlSrc))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": "that file is not a valid scenario: " + err.Error()})
		return
	}

	path := s.freePath(parsed)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "kind": kind,
		"name": parsed.Name, "path": path,
		"actors": len(parsed.Actors), "steps": len(parsed.Steps),
		"tags": parsed.Tags, "docs": len(parsed.Docs),
		"image": parsed.MySQL.Image, "isolation": parsed.MySQL.Isolation,
		"runs": len(bundle.Runs), "versions": len(bundle.Versions),
		"exported_at": bundle.Exported,
		"warnings":    agentapi.LintCase(parsed),
	})
}

// handleImportScenario accepts either a bare scenario YAML or a bundle, and
// writes it into the library.
//
// Importing never overwrites: a name that is taken gets a suffix instead. The
// alternative is a drag-and-drop that silently replaces work, which is not a
// trade anyone would take.
func (s *Server) handleImportScenario(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	yamlSrc, err := unwrapImport(body)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	parsed, err := casedef.Parse([]byte(yamlSrc))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": "that file is not a valid scenario: " + err.Error()})
		return
	}

	path := s.freePath(parsed)
	saved, err := s.lib.SaveNote(path, []byte(yamlSrc), "imported")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.api.Hub().Publish(agentapi.Activity{
		Source: agentapi.SourceUI, Kind: agentapi.KindScenarioCreated, ScenarioID: saved.ID,
		Summary: fmt.Sprintf("imported scenario %q", saved.Name),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": saved.ID, "name": saved.Name, "path": saved.Path,
	})
}

// unwrapImport pulls the YAML out of whatever was dropped: a bundle, or the
// scenario itself.
func unwrapImport(body []byte) (string, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", errors.New("that file is empty")
	}
	if !strings.HasPrefix(trimmed, "{") {
		return trimmed, nil
	}

	var bundle scenarioBundle
	if err := json.Unmarshal([]byte(trimmed), &bundle); err != nil {
		return "", fmt.Errorf("that looks like JSON but is not a scenario bundle: %w", err)
	}
	if bundle.Format != bundleFormat {
		return "", errors.New("that JSON file is not a Deadlocker scenario bundle")
	}
	if strings.TrimSpace(bundle.YAML) == "" {
		return "", errors.New("that bundle has no scenario in it")
	}
	return bundle.YAML, nil
}

// freePath finds a library path that is not taken, so importing the same
// scenario twice gives two files rather than one overwritten one.
func (s *Server) freePath(c *casedef.Case) string {
	base := agentapi.SuggestPath(c)
	if !s.lib.Exists(base) {
		if id := casedef.IDForPath(base); id == "" {
			return base
		} else if _, taken := s.lib.Get(id); !taken {
			return base
		}
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	for n := 2; n < 100; n++ {
		candidate := fmt.Sprintf("%s-%d.yaml", stem, n)
		if s.lib.Exists(candidate) {
			continue
		}
		if _, taken := s.lib.Get(casedef.IDForPath(candidate)); !taken {
			return candidate
		}
	}
	return base
}

// ------------------------------------------------------- command palette

// paletteItem is one addressable thing in the app.
type paletteItem struct {
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	Subtitle string   `json:"subtitle,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Terms    []string `json:"terms,omitempty"`
	URL      string   `json:"url"`
}

// handlePalette returns everything the command palette can jump to.
//
// The whole index is sent at once and matched in the browser: a local library
// is a few dozen scenarios and a few dozen runs, so a round trip per keystroke
// would buy nothing and cost responsiveness.
func (s *Server) handlePalette(w http.ResponseWriter, r *http.Request) {
	pd := s.base("", "")

	items := make([]paletteItem, 0, 64)
	for _, c := range s.lib.List() {
		terms := append([]string{c.ID, c.Path}, c.Tags...)
		// The description is searchable but not shown in full: matching on
		// "insert intention" should find the scenario that explains it.
		items = append(items, paletteItem{
			Kind: "scenario", Title: c.Name, Subtitle: c.Category,
			Detail: firstSentence(c.Description),
			Terms:  append(terms, c.Description),
			URL:    "/case/" + c.ID,
		})
	}
	for _, run := range pd.Runs {
		status := run.Outcome
		if run.Live {
			status = "live"
		}
		items = append(items, paletteItem{
			Kind: "run", Title: run.CaseName,
			Subtitle: "run · " + status,
			Detail:   run.RunID,
			Terms:    []string{run.RunID, run.CaseID},
			URL:      "/run/" + run.RunID,
		})
	}
	// Drafts are the most likely thing anyone is trying to get back to, since by
	// definition they are what was being worked on.
	for _, d := range pd.Drafts {
		sub := "draft"
		if d.ScenarioID != "" {
			sub = "draft · unsaved changes"
		}
		items = append(items, paletteItem{
			Kind: "draft", Title: d.Name, Subtitle: sub,
			Detail: d.UpdatedAt.Format("15:04:05"),
			Terms:  []string{d.ID, d.ScenarioID, "draft", "unsaved"},
			URL:    "/playground?draft=" + d.ID,
		})
	}
	for _, j := range s.api.Jobs().All() {
		kind := "matrix"
		if j.Kind != "isolation-matrix" {
			kind = "minimal repro"
		}
		items = append(items, paletteItem{
			Kind: "analysis", Title: j.Name,
			Subtitle: "analysis · " + kind, Detail: j.Status,
			Terms: []string{j.ID, j.ScenarioID},
			URL:   "/analysis/" + j.ID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": items})
}

// firstSentence trims a description down to something that fits on one line.
func firstSentence(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if len(s) > 120 {
		s = strings.TrimSpace(s[:120]) + "…"
	}
	return s
}

// ------------------------------------------------------- scenario versions

// handleScenarioVersions lists a scenario's revision history.
func (s *Server) handleScenarioVersions(w http.ResponseWriter, r *http.Request) {
	out, err := s.api.ListScenarioVersions(r.Context(), agentapi.ListScenarioVersionsInput{
		ID: r.PathValue("id"),
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "versions": out.Versions})
}

// handleScenarioVersion returns the YAML one revision held, for previewing
// before restoring it.
func (s *Server) handleScenarioVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.ParseUint(r.PathValue("version"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid version"})
		return
	}
	out, err := s.api.GetScenarioVersion(r.Context(), agentapi.GetScenarioVersionInput{
		ID: r.PathValue("id"), Version: version,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": out.Version, "note": out.Note, "yaml": out.YAML,
	})
}

// handleScenarioRestore writes an earlier revision back to the file.
func (s *Server) handleScenarioRestore(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.ParseUint(r.PathValue("version"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid version"})
		return
	}
	out, err := s.api.RestoreScenarioVersion(r.Context(), agentapi.RestoreScenarioVersionInput{
		ID: r.PathValue("id"), Version: version,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": out.ID, "path": out.Path})
}

// handleModels proxies a /models request so the browser can populate the model
// dropdown without needing CORS on the model server.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// An empty key in the request means "use the stored one".
	if req.APIKey == "" {
		if cfg, _, err := s.store.Current(); err == nil {
			req.APIKey = cfg.LLM.APIKey
		}
	}
	models, err := chat.FetchModels(r.Context(), req.BaseURL, req.APIKey)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models})
}

// -------------------------------------------------------------------- chat

func (s *Server) handleChatStatus(w http.ResponseWriter, r *http.Request) {
	cfg, _, err := s.store.Current()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ready": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":    cfg.Ready(),
		"enabled":  cfg.LLM.Enabled,
		"model":    cfg.LLM.Model,
		"base_url": cfg.LLM.BaseURL,
	})
}

func (s *Server) handleChatNew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode       string `json:"mode"`
		ScenarioID string `json:"scenario_id"`
		RunID      string `json:"run_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	mode := chat.ModeDiscuss
	if req.Mode == string(chat.ModeBuild) {
		mode = chat.ModeBuild
	}

	draft := ""
	if mode == chat.ModeBuild {
		// Editing an existing scenario starts from its current source;
		// authoring from scratch starts from a runnable skeleton.
		if req.ScenarioID != "" {
			if c, ok := s.lib.Get(req.ScenarioID); ok {
				draft = c.Source
			}
		}
		draft = chat.EnsureValidDraft(draft)
	}

	id, err := newID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sess := s.chat.NewSession(id, mode, req.ScenarioID, req.RunID, draft)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "session": sess.ID, "mode": string(sess.Mode),
		"draft": sess.DraftYAML(), "scenario": chat.ParseDraft(sess.DraftYAML()),
	})
}

// handleChatSend streams one assistant turn back as server-sent events on the
// POST response, so text, tool calls and draft updates all arrive as they
// happen rather than in one lump at the end.
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.chat.Session(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown chat session"})
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	emit := func(ev chat.Event) {
		payload, err := json.Marshal(ev)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
		flusher.Flush()
	}

	// A generation can outlive the request only if the client goes away; give
	// it a hard ceiling either way.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	if err := s.chat.Send(ctx, sess, req.Message, emit); err != nil {
		emit(chat.Event{Type: "error", Message: err.Error()})
	}
}

func (s *Server) handleChatSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.chat.Session(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown chat session"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "session": sess.ID, "mode": string(sess.Mode),
		"scenario_id": sess.ScenarioID, "run_id": sess.RunID,
		"draft": sess.DraftYAML(), "scenario": chat.ParseDraft(sess.DraftYAML()),
		"transcript": sess.Transcript,
	})
}

// handleChatDiscard ends a session. Closing the builder is a decision to walk
// away, so the conversation should not be waiting when it is next opened.
func (s *Server) handleChatDiscard(w http.ResponseWriter, r *http.Request) {
	s.chat.Discard(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleChatDraft lets the human edit the draft the assistant is working on,
// keeping both sides on the same document.
func (s *Server) handleChatDraft(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.chat.Session(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown chat session"})
		return
	}
	var req struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if _, err := casedef.Parse([]byte(req.YAML)); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sess.SetDraft(req.YAML)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scenario": chat.ParseDraft(req.YAML)})
}

// handleChatSaveDraft writes the current draft into the library.
func (s *Server) handleChatSaveDraft(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.chat.Session(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown chat session"})
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)

	ctx := agentapi.WithSource(r.Context(), agentapi.SourceUI)
	draft := sess.DraftYAML()

	var (
		out agentapi.SaveScenarioOutput
		err error
	)
	if sess.ScenarioID != "" {
		out, err = s.api.UpdateScenario(ctx, agentapi.UpdateScenarioInput{ID: sess.ScenarioID, YAML: draft})
	} else {
		out, err = s.api.CreateScenario(ctx, agentapi.CreateScenarioInput{YAML: draft, Path: req.Path})
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sess.SetScenarioID(out.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": out.ID, "path": out.Path, "warnings": out.Warnings})
}

func newID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// handleChatPrompts returns a random handful of starting suggestions. The
// selection happens per request so opening the builder twice offers different
// ideas rather than the same three every time.
func (s *Server) handleChatPrompts(w http.ResponseWriter, r *http.Request) {
	mode := chat.ModeBuild
	if r.URL.Query().Get("mode") == string(chat.ModeDiscuss) {
		mode = chat.ModeDiscuss
	}
	n := 3
	if v := r.URL.Query().Get("n"); v != "" {
		fmt.Sscanf(v, "%d", &n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"prompts": chat.SamplePrompts(mode, n)})
}

// ------------------------------------------------------- background analyses

// handleAnalyse starts an isolation-level sweep or a minimal-repro reduction.
// Both run many real MySQL runs, so they return a job id immediately and the
// browser polls.
func (s *Server) handleAnalyse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ScenarioID string   `json:"scenario_id"`
		YAML       string   `json:"yaml"`
		Target     string   `json:"target"`
		Images     []string `json:"images,omitempty"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)

	ctx := agentapi.WithSource(r.Context(), agentapi.SourceUI)
	var (
		out agentapi.JobStartedOutput
		err error
	)
	switch r.PathValue("kind") {
	case "isolation":
		out, err = s.api.StartIsolationMatrix(ctx, agentapi.IsolationMatrixInput{
			ScenarioID: req.ScenarioID, YAML: req.YAML,
		})
	case "version":
		out, err = s.api.StartVersionMatrix(ctx, agentapi.VersionMatrixInput{
			ScenarioID: req.ScenarioID, YAML: req.YAML, Images: req.Images,
		})
	case "shrink":
		out, err = s.api.StartShrink(ctx, agentapi.ShrinkInput{
			ScenarioID: req.ScenarioID, YAML: req.YAML, Target: req.Target,
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown analysis"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": out.JobID})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	out, err := s.api.GetJob(r.Context(), agentapi.GetJobInput{JobID: r.PathValue("id")})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": out.Job})
}
