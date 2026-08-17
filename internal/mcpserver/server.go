// Package mcpserver exposes the shared operation layer over MCP, using the
// streamable HTTP transport.
//
// Every tool here is a thin adapter over internal/agentapi, which is the same
// code the built-in chat drives. Adding an ability in one place makes it
// available to both.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jnk/deadlocker/internal/agentapi"
)

// Version is reported to MCP clients.
const Version = "1.0.0"

// New builds the MCP server with every tool and resource registered.
func New(api *agentapi.API) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "deadlocker",
		Title:   "Deadlocker — MySQL lock playground",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	registerTools(srv, api)
	registerResources(srv, api)
	return srv
}

const instructions = `Deadlocker provokes and explains MySQL locking behaviour.

A scenario is a YAML document describing a schema, a set of actors (each with
its own MySQL connection) and an ordered list of statements. Running one steps
through those statements while reporting which locks are taken and who blocks
whom.

Typical loop:
  1. list_scenarios / get_scenario to see what exists.
  2. start_run to prepare a run.
  3. step_run repeatedly. Each call returns the step outcome AND the resulting
     lock state, including a wait-for cycle flag.
  4. close_run when finished, which drops the scratch database.

When authoring a scenario, always validate_scenario before create_scenario or
update_scenario, and read the deadlocker://docs/format resource first.

Important constraint: an actor whose statement is blocked cannot run its next
statement, because a real connection runs one statement at a time. Order steps
so the releasing COMMIT or ROLLBACK comes before the blocked actor's next step.`

// tool registers one operation, wiring the handler so that structured output
// is returned to the client and errors come back as readable tool errors rather
// than protocol failures.
func tool[In, Out any](
	srv *mcp.Server,
	name, title, description string,
	fn func(context.Context, In) (Out, error),
) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		ctx = agentapi.WithSource(ctx, agentapi.SourceMCP)
		out, err := fn(ctx, in)
		if err != nil {
			// A failed operation is a normal outcome the model should see and
			// react to, not a transport error.
			var zero Out
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, zero, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(out)}},
		}, out, nil
	})
}

func registerTools(srv *mcp.Server, api *agentapi.API) {
	tool(srv, "list_scenarios", "List scenarios",
		"List the scenario library, optionally filtered by category, tag or free text.",
		api.ListScenarios)

	tool(srv, "get_scenario", "Get a scenario",
		"Fetch one scenario in full, including its YAML source, actors and steps.",
		api.GetScenario)

	tool(srv, "validate_scenario", "Validate scenario YAML",
		"Parse scenario YAML without saving it. Returns a precise error when invalid, "+
			"plus warnings about structural problems that parse but will not run as intended. "+
			"Always call this before creating or updating.",
		api.ValidateScenario)

	tool(srv, "create_scenario", "Create a scenario",
		"Write a new scenario to the library. Fails if a scenario with the same id already exists.",
		api.CreateScenario)

	tool(srv, "update_scenario", "Update a scenario",
		"Replace an existing scenario's YAML in place. Validates before writing.",
		api.UpdateScenario)

	tool(srv, "start_run", "Start a run",
		"Prepare a run from a saved scenario (scenario_id) or from ad-hoc YAML. "+
			"Creates a scratch database and one MySQL connection per actor. "+
			"The run starts at step zero; use step_run to advance it.",
		api.StartRun)

	tool(srv, "step_run", "Step a run",
		"Submit the next statement (or several, via count). Each returns the step outcome "+
			"together with the lock state it produced: lock modes with plain-language "+
			"explanations, wait edges, and whether the wait graph contains a cycle. "+
			"Stops early and explains why if an actor is blocked.",
		api.StepRun)

	tool(srv, "run_all", "Run a scenario to the end",
		"Advance a run until the scenario ends or an actor is stuck waiting on a lock.",
		api.RunAll)

	tool(srv, "get_run", "Get run state",
		"Current status, every step's result, and the live lock state for a run.",
		api.GetRun)

	tool(srv, "get_locks", "Read current locks",
		"Re-read performance_schema.data_locks and data_lock_waits for a run.",
		api.GetLocks)

	tool(srv, "close_run", "Close a run",
		"Tear a run down, releasing its connections and dropping its scratch database.",
		api.CloseRun)

	tool(srv, "list_history", "List past runs",
		"Recorded runs, newest first, with per-run counts of blocked steps, deadlocks, "+
			"timeouts and expectation mismatches.",
		api.ListHistory)

	tool(srv, "compare_runs", "Compare two runs",
		"Diff two recorded runs step by step, reporting only what actually differed.",
		api.CompareRuns)
}

func registerResources(srv *mcp.Server, api *agentapi.API) {
	srv.AddResource(&mcp.Resource{
		URI:         "deadlocker://docs/format",
		Name:        "scenario-format",
		Title:       "Scenario YAML format",
		Description: "The complete scenario file format, with the rules that matter when authoring one.",
		MIMEType:    "text/markdown",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return textResource(req.Params.URI, "text/markdown", agentapi.FormatDoc), nil
	})

	srv.AddResource(&mcp.Resource{
		URI:         "deadlocker://scenarios",
		Name:        "scenarios",
		Title:       "Scenario library",
		Description: "Every scenario in the library, summarised.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		out, err := api.ListScenarios(agentapi.WithSource(ctx, agentapi.SourceMCP), agentapi.ListScenariosInput{})
		if err != nil {
			return nil, err
		}
		return textResource(req.Params.URI, "application/json", mustJSON(out)), nil
	})

	srv.AddResource(&mcp.Resource{
		URI:         "deadlocker://history",
		Name:        "history",
		Title:       "Run history",
		Description: "Recorded runs with their outcomes.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		out, err := api.ListHistory(agentapi.WithSource(ctx, agentapi.SourceMCP), agentapi.ListHistoryInput{Limit: 100})
		if err != nil {
			return nil, err
		}
		return textResource(req.Params.URI, "application/json", mustJSON(out)), nil
	})

	// Templates let a client address an individual scenario or run by id.
	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "deadlocker://scenario/{id}",
		Name:        "scenario",
		Title:       "A scenario's YAML source",
		MIMEType:    "application/yaml",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		id := strings.TrimPrefix(req.Params.URI, "deadlocker://scenario/")
		out, err := api.GetScenario(agentapi.WithSource(ctx, agentapi.SourceMCP), agentapi.GetScenarioInput{ID: id})
		if err != nil {
			return nil, err
		}
		return textResource(req.Params.URI, "application/yaml", out.YAML), nil
	})

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "deadlocker://run/{id}",
		Name:        "run",
		Title:       "A run's state and locks",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		id := strings.TrimPrefix(req.Params.URI, "deadlocker://run/")
		out, err := api.GetRun(agentapi.WithSource(ctx, agentapi.SourceMCP), agentapi.GetRunInput{RunID: id})
		if err != nil {
			return nil, err
		}
		return textResource(req.Params.URI, "application/json", mustJSON(out)), nil
	})
}

func textResource(uri, mime, text string) *mcp.ReadResourceResult {
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Text: text}},
	}
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("{\"error\":%q}", err.Error())
	}
	return string(b)
}

// Handler returns an HTTP handler serving MCP over the streamable HTTP
// transport. A fresh server instance per request keeps sessions isolated.
func Handler(api *agentapi.API) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return New(api)
	}, nil)
}
