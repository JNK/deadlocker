// Package web serves the server-rendered UI. Pages are rendered with
// html/template; a small amount of vanilla JavaScript subscribes to a
// server-sent event stream and patches the live regions. There is no build
// step and no npm dependency -- the binary is the whole application.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jnk/deadlocker/internal/agentapi"
	"github.com/jnk/deadlocker/internal/casedef"
	"github.com/jnk/deadlocker/internal/chat"
	"github.com/jnk/deadlocker/internal/engine"
	"github.com/jnk/deadlocker/internal/markdown"
	"github.com/jnk/deadlocker/internal/mysqlbox"
	"github.com/jnk/deadlocker/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server wires the case library, the run manager, the shared operation layer,
// the assistant and the MCP endpoint to HTTP.
type Server struct {
	lib   *casedef.Library
	mgr   *engine.Manager
	pool  *mysqlbox.Pool
	api   *agentapi.API
	chat  *chat.Service
	store *store.Store
	mcp   http.Handler
	tmpl  *template.Template
	start time.Time

	// quit is closed on shutdown. Server-sent event streams watch it so
	// http.Server.Shutdown does not have to wait out its whole timeout for
	// long-lived connections that would otherwise never finish on their own.
	quit chan struct{}
	once sync.Once
}

// Deps bundles everything the HTTP layer needs.
type Deps struct {
	Library *casedef.Library
	Manager *engine.Manager
	Pool    *mysqlbox.Pool
	API     *agentapi.API
	Chat    *chat.Service
	Store   *store.Store
	MCP     http.Handler
}

func NewServer(d Deps) (*Server, error) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{
		lib: d.Library, mgr: d.Manager, pool: d.Pool,
		api: d.API, chat: d.Chat, store: d.Store, mcp: d.MCP,
		tmpl: tmpl, start: time.Now(), quit: make(chan struct{}),
	}, nil
}

// Shutdown releases every streaming request. Call it before
// http.Server.Shutdown.
func (s *Server) Shutdown() {
	s.once.Do(func() { close(s.quit) })
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"json": func(v any) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("null")
			}
			return template.JS(b)
		},
		"join":      strings.Join,
		"upper":     strings.ToUpper,
		"lower":     strings.ToLower,
		"trim":      strings.TrimSpace,
		"add":       func(a, b int) int { return a + b },
		"hasPrefix": strings.HasPrefix,
		"firstLine": func(s string) string {
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				return s[:i]
			}
			return s
		},
		"shortSQL": func(s string) string {
			s = strings.Join(strings.Fields(s), " ")
			if len(s) > 90 {
				return s[:89] + "…"
			}
			return s
		},
		"clock": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("15:04:05")
		},
		"dateTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("2 Jan 15:04:05")
		},
		"since": humanDuration,
		// Optional settings render as an empty field, which reads as
		// "unset — use the model server's default".
		"optFloat": func(v *float64) string {
			if v == nil {
				return ""
			}
			return strconv.FormatFloat(*v, 'g', -1, 64)
		},
		"optInt": func(v *int) string {
			if v == nil {
				return ""
			}
			return strconv.Itoa(*v)
		},
		"optInt64": func(v *int64) string {
			if v == nil {
				return ""
			}
			return strconv.FormatInt(*v, 10)
		},
		// A tag always gets the same tone, so "gap-lock" reads the same colour
		// everywhere it appears.
		"tagTone": func(tag string) int {
			h := fnv.New32a()
			_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(tag))))
			return int(h.Sum32()%8) + 1
		},
		// Outcomes are slugs so they can be class names; this is how they read.
		"outcomeLabel": func(s string) string {
			return strings.ReplaceAll(s, "-", " ")
		},
		"markdown": func(s string) template.HTML {
			return markdown.Render(s)
		},
	}
}

// humanDuration renders a duration at a resolution a human cares about.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	static, err := newStaticHandler()
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /static/", static)

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /builder", s.handleBuilder)
	mux.HandleFunc("GET /case/{id}", s.handleCase)
	mux.HandleFunc("GET /compare", s.handleCompare)
	mux.HandleFunc("GET /api/history", s.handleHistoryAPI)
	mux.HandleFunc("GET /api/runs", s.handleRunsAPI)
	mux.HandleFunc("GET /api/jobs", s.handleJobsAPI)
	mux.HandleFunc("POST /api/runs/clear", s.handleClearRuns)
	mux.HandleFunc("POST /api/runs/{id}/forget", s.handleForgetRun)
	mux.HandleFunc("GET /analysis/{id}", s.handleAnalysisPage)
	mux.HandleFunc("POST /api/analysis/{id}/apply", s.handleApplyShrink)
	mux.HandleFunc("POST /api/analyse/{kind}", s.handleAnalyse)
	mux.HandleFunc("GET /api/job/{id}", s.handleJob)
	mux.HandleFunc("GET /playground", s.handlePlayground)
	mux.HandleFunc("POST /playground/validate", s.handleValidate)
	mux.HandleFunc("POST /playground/save", s.handleSave)

	mux.HandleFunc("POST /run", s.handleStartRun)
	mux.HandleFunc("GET /run/{id}", s.handleRunPage)
	mux.HandleFunc("GET /run/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /run/{id}/step", s.handleStep)
	mux.HandleFunc("POST /run/{id}/play", s.handlePlay)
	mux.HandleFunc("POST /run/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /run/{id}/stop", s.handleStopRun)
	mux.HandleFunc("POST /run/{id}/close", s.handleCloseRun)
	mux.HandleFunc("GET /run/{id}/state", s.handleRunState)
	mux.HandleFunc("GET /run/{id}/export", s.handleExport)

	// The scenario editor writes back to the same file rather than forcing a
	// clone.
	mux.HandleFunc("GET /edit/{id}", s.handleEdit)

	// The command palette's whole index, matched in the browser.
	mux.HandleFunc("GET /api/palette", s.handlePalette)

	// Scenario history. Every save is recorded, so an edit that turns out to be
	// wrong is one click away from being undone.
	mux.HandleFunc("GET /api/case/{id}/versions", s.handleScenarioVersions)
	mux.HandleFunc("GET /api/case/{id}/versions/{version}", s.handleScenarioVersion)
	mux.HandleFunc("POST /api/case/{id}/versions/{version}/restore", s.handleScenarioRestore)

	mux.HandleFunc("GET /settings", s.handleSettingsPage)
	mux.HandleFunc("POST /api/settings", s.handleSettingsSave)
	mux.HandleFunc("POST /api/settings/restore", s.handleSettingsRestore)
	mux.HandleFunc("POST /api/models", s.handleModels)

	mux.HandleFunc("GET /api/activity", s.handleActivity)
	mux.HandleFunc("GET /api/chat/status", s.handleChatStatus)
	mux.HandleFunc("GET /api/chat/prompts", s.handleChatPrompts)
	mux.HandleFunc("POST /api/chat", s.handleChatNew)
	mux.HandleFunc("GET /api/chat/{id}", s.handleChatSession)
	mux.HandleFunc("POST /api/chat/{id}/send", s.handleChatSend)
	mux.HandleFunc("POST /api/chat/{id}/discard", s.handleChatDiscard)
	mux.HandleFunc("POST /api/chat/{id}/draft", s.handleChatDraft)
	mux.HandleFunc("POST /api/chat/{id}/save", s.handleChatSaveDraft)

	// MCP over streamable HTTP. External clients get exactly the same
	// operations the built-in assistant uses.
	if s.mcp != nil {
		mux.Handle("/mcp", s.mcp)
		mux.Handle("/mcp/", s.mcp)
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uptime": time.Since(s.start).String()})
	})

	return mux
}

// handleEdit opens the editor on an existing scenario, saving in place.
func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	pd := s.base("", "library")
	c, ok := s.lib.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pd.Title = "Edit " + c.Name
	pd.Case = c
	pd.ActiveCase = c.ID
	pd.Source = c.Source
	pd.Editing = true
	s.render(w, "playground.html", pd)
}

func newStaticHandler() (http.Handler, error) {
	// The embedded paths already start with "static/", so the URL prefix maps
	// straight through without a sub-filesystem.
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assets are embedded and change only with the binary, but a running
		// dev loop benefits from revalidation.
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	}), nil
}

// ---------------------------------------------------------------- page data

type pageData struct {
	Title      string
	Nav        string
	Categories []casedef.Category
	Broken     map[string]string
	Runs       []historyEntry

	Case        *casedef.Case
	Source      string
	Description template.HTML
	// VersionCount is how many revisions the scenario has, shown on the tab.
	VersionCount int

	Sequence []sequenceRow
	Run      *engine.RunState
	// Archived marks a run that has finished and been closed: it is rendered
	// from its history record, with no live stream and no controls.
	Archived bool
	// ArchivedOutcome is the recorded one-word verdict. The page shows it so it
	// cannot disagree with what the sidebar says about the same run.
	ArchivedOutcome string
	ArchivedLocks   *engine.LockSnapshot
	Steps           []*engine.StepResult
	CaseSteps       []casedef.Step
	Settle          int64

	Analyses   []*agentapi.Job
	Job        *agentapi.Job
	History    []*engine.Record
	AllHistory []*engine.Record
	Diff       *engine.DiffResult

	// Editing switches the editor from "save a copy" to "save in place".
	Editing bool
	// ActiveCase highlights the scenario this page belongs to in the sidebar.
	ActiveCase string
	// ActiveRun highlights the run being viewed.
	ActiveRun string
	// OpenBuilder opens the assistant sheet as soon as the page loads.
	OpenBuilder bool

	Config         *store.Config
	ConfigVersion  uint64
	ConfigVersions []store.Version
	HasAPIKey      bool
	StorePath      string
	MCPURL         string

	ChatReady bool
	ChatModel string

	Error   string
	Message string
}

// historyEntry is one line in the sidebar: a run, live or finished.
// sequenceRow is one step of a scenario, resolved against its actor so the
// template does not have to look colours up.
type sequenceRow struct {
	Index  int
	Actor  string
	Name   string
	Accent string
	Label  string
	SQL    string
	Note   string
	Expect string
}

type historyEntry struct {
	RunID     string    `json:"run_id"`
	CaseID    string    `json:"case_id"`
	CaseName  string    `json:"case_name"`
	Status    string    `json:"status"`
	Outcome   string    `json:"outcome,omitempty"`
	Cursor    int       `json:"cursor"`
	Total     int       `json:"total"`
	StartedAt time.Time `json:"started_at"`
	Live      bool      `json:"live"`
}

// sidebarRunLimit is how many runs the log shows. It matches the engine's own
// retention cap, so the sidebar shows everything that is still kept rather than
// a window onto it -- the list is virtual-scrolled by the browser and reconciled
// by id, so length costs little.
const sidebarRunLimit = 500

func (s *Server) base(title, nav string) *pageData {
	_ = s.lib.Load()
	pd := &pageData{
		Title:      title,
		Nav:        nav,
		Categories: s.lib.Categories(),
		Broken:     s.lib.Broken(),
		Settle:     s.mgr.SettleWindow().Milliseconds(),
	}
	// Every page needs this: the assistant's entry points are hidden entirely
	// until it is configured, rather than offered and then failing.
	if cfg, _, err := s.store.Current(); err == nil {
		pd.ChatReady = cfg.Ready()
		pd.ChatModel = cfg.LLM.Model
	}
	// The sidebar is a run log: live runs first, then recent finished ones.
	live := map[string]bool{}
	for _, r := range s.mgr.List() {
		st := r.State()
		// Draft runs belong to the builder, not to the library.
		if st.Ephemeral {
			continue
		}
		live[st.ID] = true
		pd.Runs = append(pd.Runs, historyEntry{
			RunID: st.ID, CaseID: st.CaseID, CaseName: st.CaseName,
			Status: st.Status, Cursor: st.Cursor, Total: st.Total,
			StartedAt: st.Started, Live: true,
		})
	}
	for _, rec := range s.mgr.History().All() {
		if rec.Ephemeral || live[rec.RunID] {
			continue
		}
		pd.Runs = append(pd.Runs, historyEntry{
			RunID: rec.RunID, CaseID: rec.CaseID, CaseName: rec.CaseName,
			Status: rec.Status, Outcome: rec.Outcome,
			Cursor: rec.Submitted, Total: len(rec.Steps), StartedAt: rec.StartedAt,
		})
		if len(pd.Runs) >= sidebarRunLimit {
			break
		}
	}

	// Analyses are listed beside the runs: each is a batch of runs with a
	// conclusion, which is worth keeping visible while it works.
	for i, job := range s.api.Jobs().All() {
		if i >= 12 {
			break
		}
		pd.Analyses = append(pd.Analyses, job)
	}
	return pd
}

func (s *Server) render(w http.ResponseWriter, name string, data *pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		// The response is likely already partially written; log-shaped output
		// in the page is more useful than a silent truncation.
		fmt.Fprintf(w, "\n<!-- template error: %v -->", err)
	}
}

// ------------------------------------------------------------------ handlers

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	pd := s.base("Scenarios", "library")
	s.render(w, "index.html", pd)
}

// handleBuilder is the assistant builder as a navigable page. It renders the
// library and opens the builder sheet over it, so the URL is shareable and
// closing the sheet lands on the scenario list.
func (s *Server) handleBuilder(w http.ResponseWriter, r *http.Request) {
	pd := s.base("Scenario builder", "library")
	pd.OpenBuilder = true
	if from := r.URL.Query().Get("from"); from != "" {
		if c, ok := s.lib.Get(from); ok {
			pd.Case = c
			pd.ActiveCase = c.ID
		}
	}
	s.render(w, "index.html", pd)
}

func (s *Server) handleCase(w http.ResponseWriter, r *http.Request) {
	// base() rescans the library, so the lookup has to come after it. Reading
	// the case first served a stale copy whenever the file had changed since
	// the last load -- which is every time a scenario is edited or replaced.
	pd := s.base("", "library")

	c, ok := s.lib.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pd.Title = c.Name
	pd.Case = c
	pd.ActiveCase = c.ID
	pd.Source = c.Source
	pd.Description = markdown.Render(c.Description)
	pd.History = s.mgr.History().ForCase(c.ID)
	// The count goes on the tab so the history is visible without opening it;
	// the revisions themselves load only when the tab is.
	if counts, err := s.store.ScenarioVersionCounts(); err == nil {
		pd.VersionCount = counts[c.ID]
	}
	for i, step := range c.Steps {
		actor, _ := c.Actor(step.Actor)
		pd.Sequence = append(pd.Sequence, sequenceRow{
			Index: i + 1, Actor: step.Actor, Name: actor.Name, Accent: actor.Accent,
			Label: step.Label, SQL: strings.TrimSpace(step.SQL),
			Note: step.Note, Expect: string(step.Expect),
		})
	}
	s.render(w, "case.html", pd)
}

// handleCompare shows two runs side by side. With no run ids it renders a
// picker over the history instead.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	pd := s.base("Compare runs", "compare")
	pd.AllHistory = s.mgr.History().All()

	aID := r.URL.Query().Get("a")
	bID := r.URL.Query().Get("b")
	if aID == "" || bID == "" {
		s.render(w, "compare.html", pd)
		return
	}

	a, okA := s.mgr.History().Get(aID)
	b, okB := s.mgr.History().Get(bID)
	if !okA || !okB {
		pd.Error = "One of those runs is no longer in the history. Runs are kept in memory only, and the oldest are dropped once the log is full."
		s.render(w, "compare.html", pd)
		return
	}

	diff := engine.Diff(a, b)
	pd.Diff = &diff
	pd.Title = "Compare · " + a.CaseName
	s.render(w, "compare.html", pd)
}

// handleHistoryAPI serves the run log as JSON, for the run page's history menu.
func (s *Server) handleHistoryAPI(w http.ResponseWriter, r *http.Request) {
	if caseID := r.URL.Query().Get("case"); caseID != "" {
		writeJSON(w, http.StatusOK, map[string]any{"runs": s.mgr.History().ForCase(caseID)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.mgr.History().All()})
}

func (s *Server) handlePlayground(w http.ResponseWriter, r *http.Request) {
	pd := s.base("Playground", "playground")
	pd.Source = starterYAML
	if from := r.URL.Query().Get("from"); from != "" {
		if c, ok := s.lib.Get(from); ok {
			pd.Source = c.Source
			pd.Message = "Loaded from " + c.Name + ". Edits here do not touch the file until you save."
		}
	}
	s.render(w, "playground.html", pd)
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	c, err := casedef.Parse(body)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"name":   c.Name,
		"actors": len(c.Actors),
		"steps":  len(c.Steps),
		"image":  c.MySQL.Image,
		// The parsed shape drives the step pane beside the editor.
		"scenario": chat.ParseDraft(string(body)),
	})
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		Source string `json:"source"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "a file name is required"})
		return
	}
	note := strings.TrimSpace(req.Note)
	if note == "" {
		note = "edited in the browser"
	}
	c, err := s.lib.SaveNote(req.Path, []byte(req.Source), note)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": c.ID, "path": c.Path})
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var (
		c   *casedef.Case
		err error
	)

	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		var req struct {
			Source string `json:"source"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		c, err = casedef.Parse([]byte(req.Source))
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		// Ad-hoc YAML from the playground or the builder draft: the run is real,
		// but no saved scenario owns it, so it must not surface in the sidebar
		// or in any scenario's history.
		c.Ephemeral = true
		if c.ID == "" {
			c.ID = "draft"
		}
		s.startAndRespond(w, r, c, true)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	caseID := r.FormValue("case_id")
	c, ok := s.lib.Get(caseID)
	if !ok {
		http.Error(w, "unknown case "+caseID, http.StatusNotFound)
		return
	}
	s.startAndRespond(w, r, c, false)
}

func (s *Server) startAndRespond(w http.ResponseWriter, r *http.Request, c *casedef.Case, wantJSON bool) {
	// Container pull and MySQL boot can take a while on the very first run.
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()

	run, err := s.mgr.Start(ctx, c)
	if err != nil {
		if wantJSON {
			id := ""
			if run != nil {
				id = run.ID
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "run_id": id})
			return
		}
		pd := s.base("Run failed", "library")
		pd.Case = c
		pd.Error = err.Error()
		s.render(w, "error.html", pd)
		return
	}
	if wantJSON {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "run_id": run.ID})
		return
	}
	http.Redirect(w, r, "/run/"+run.ID, http.StatusSeeOther)
}

func (s *Server) handleRunPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, ok := s.mgr.Get(id)
	if !ok {
		// A closed run leaves the manager but stays in the history, and its
		// recorded result is still worth looking at. Rendering that beats a 404
		// from a link the sidebar itself offered.
		if rec, found := s.mgr.History().Get(id); found {
			s.renderArchivedRun(w, rec)
			return
		}
		http.NotFound(w, r)
		return
	}
	st := run.State()
	pd := s.base(st.CaseName, "run")
	pd.ActiveCase = st.CaseID
	pd.ActiveRun = st.ID
	pd.Run = &st
	pd.Steps = run.Steps()
	pd.Case = run.Case
	pd.CaseSteps = run.Case.Steps
	pd.Settle = s.mgr.SettleWindow().Milliseconds()
	pd.History = s.mgr.History().ForCase(st.CaseID)
	s.render(w, "run.html", pd)
}

func (s *Server) handleStep(w http.ResponseWriter, r *http.Request) {
	run, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown run"})
		return
	}
	res, err := run.Step(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, stepErrorPayload(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "step": res})
}

// handlePlay advances repeatedly until the scenario ends or an actor is stuck.
// It deliberately stops at the first blocked actor rather than pushing through:
// the pause is the interesting part.
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	run, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown run"})
		return
	}
	advanced := 0
	for {
		if _, err := run.Step(r.Context()); err != nil {
			if errors.Is(err, engine.ErrNoMoreSteps) {
				break
			}
			var blocked *engine.ActorBlockedError
			if errors.As(err, &blocked) {
				writeJSON(w, http.StatusOK, map[string]any{
					"ok": true, "advanced": advanced, "stopped": blocked.Error(),
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "advanced": advanced})
			return
		}
		advanced++
		if advanced > 500 {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "advanced": advanced})
}

func stepErrorPayload(err error) map[string]any {
	payload := map[string]any{"ok": false, "error": err.Error()}
	if errors.Is(err, engine.ErrNoMoreSteps) {
		payload["done"] = true
	}
	var blocked *engine.ActorBlockedError
	if errors.As(err, &blocked) {
		payload["blocked_actor"] = blocked.Actor
	}
	return payload
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	run, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown run"})
		return
	}
	snap := run.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "locks": snap})
}

// handleStopRun halts a run without tearing it down. When the assistant is the
// one stepping it, its next tool call comes back saying a person intervened.
func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	out, err := s.api.StopRun(agentapi.WithSource(r.Context(), agentapi.SourceUI),
		agentapi.StopRunInput{RunID: r.PathValue("id"), Reason: "stopped by the user from the UI"})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopped": out.Stopped})
}

func (s *Server) handleCloseRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.CloseRun(id); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRunState is a light snapshot of a run: its state and every step's
// current status. The builder reconciles against this so a missed or dropped
// event cannot leave the step list and the counter stale.
func (s *Server) handleRunState(w http.ResponseWriter, r *http.Request) {
	run, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown run"})
		return
	}
	st := run.State()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": st, "steps": run.Steps()})
}

// handleExport dumps the full event log, which is handy for sharing a
// reproduction or diffing two runs.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	run, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	st := run.State()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-%s.json", st.CaseID, st.ID))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"case":   run.Case,
		"state":  st,
		"steps":  run.Steps(),
		"events": run.Bus.History(0),
	})
}

// handleEvents streams the run's event bus as server-sent events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	run, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
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

	// Last-Event-ID lets a reconnecting browser resume without replaying
	// everything it already rendered.
	since := 0
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		fmt.Sscanf(v, "%d", &since)
	} else if v := r.URL.Query().Get("since"); v != "" {
		fmt.Sscanf(v, "%d", &since)
	}

	ch, backlog, cancel := run.Bus.Subscribe(since)
	defer cancel()

	send := func(ev engine.Event) bool {
		payload, err := json.Marshal(ev)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for _, ev := range backlog {
		if !send(ev) {
			return
		}
	}
	// Tell the client the backlog is done so it can drop its loading state.
	fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case ev, open := <-ch:
			if !open {
				fmt.Fprintf(w, "event: closed\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			if !send(ev) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleRunsAPI serves the sidebar's run list, so it can be refreshed in place
// rather than only at page load.
func (s *Server) handleRunsAPI(w http.ResponseWriter, r *http.Request) {
	pd := s.base("", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "runs": pd.Runs})
}

// renderArchivedRun shows a finished run from its history record. Actors are
// reconstructed from the steps, which each carry their actor's name and colour,
// so no separate actor list has to be retained.
func (s *Server) renderArchivedRun(w http.ResponseWriter, rec *engine.Record) {
	st := engine.RunState{
		ID: rec.RunID, CaseID: rec.CaseID, CaseName: rec.CaseName,
		Status: "closed", Image: rec.Image, Isolation: rec.Isolation,
		LockWaitTimeout: rec.LockWaitTimeout, Started: rec.StartedAt,
		Total: len(rec.Steps), Cursor: rec.Submitted,
		DeadlockReport: rec.DeadlockReport,
	}

	seen := map[string]bool{}
	for _, step := range rec.Steps {
		if step.Actor == "" || seen[step.Actor] {
			continue
		}
		seen[step.Actor] = true
		st.Actors = append(st.Actors, engine.ActorState{
			ID: step.Actor, Name: step.ActorName, Accent: step.Accent,
		})
	}

	pd := s.base(rec.CaseName, "run")
	pd.ActiveCase = rec.CaseID
	pd.ActiveRun = rec.RunID
	pd.Run = &st
	pd.Steps = rec.Steps
	pd.Archived = true
	pd.ArchivedOutcome = rec.Outcome
	pd.ArchivedLocks = rec.FinalLocks
	pd.Settle = s.mgr.SettleWindow().Milliseconds()
	pd.History = s.mgr.History().ForCase(rec.CaseID)
	if c, ok := s.lib.Get(rec.CaseID); ok {
		pd.Case = c
		pd.CaseSteps = c.Steps
	}
	s.render(w, "run.html", pd)
}

// handleJobsAPI serves the analysis list for the sidebar.
func (s *Server) handleJobsAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobs": s.api.Jobs().All()})
}

// handleAnalysisPage renders one analysis result on its own URL, so a finding
// can be linked to rather than only existing inside a tab.
func (s *Server) handleAnalysisPage(w http.ResponseWriter, r *http.Request) {
	out, err := s.api.GetJob(r.Context(), agentapi.GetJobInput{JobID: r.PathValue("id")})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pd := s.base(out.Job.Name, "analysis")
	pd.Job = out.Job
	pd.ActiveCase = out.Job.ScenarioID
	s.render(w, "analysis.html", pd)
}

// handleApplyShrink writes a minimal reproduction into the library, either over
// the scenario it came from or as a new one beside it.
func (s *Server) handleApplyShrink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
		Path string `json:"path"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)

	out, err := s.api.ApplyShrink(agentapi.WithSource(r.Context(), agentapi.SourceUI),
		agentapi.ApplyShrinkInput{JobID: r.PathValue("id"), Mode: req.Mode, Path: req.Path})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": out.ID, "path": out.Path, "warnings": out.Warnings,
	})
}
