// Command mcpprobe exercises the Deadlocker MCP server as a real client.
//
// It connects over streamable HTTP, lists tools and resources, then drives a
// full scenario lifecycle: author, validate, create, run, step, inspect locks
// and close. Run it against a live server:
//
//	go run ./hack/mcpprobe http://127.0.0.1:8899/mcp
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	url := "http://127.0.0.1:8899/mcp"
	if len(os.Args) > 1 {
		url = os.Args[1]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "mcpprobe", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer session.Close()

	fmt.Println("connected to", url)

	// ---------------------------------------------------------- discovery
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		log.Fatalf("list tools: %v", err)
	}
	fmt.Printf("\ntools (%d):\n", len(tools.Tools))
	for _, t := range tools.Tools {
		fmt.Printf("  %-20s %s\n", t.Name, firstLine(t.Description))
	}

	resources, err := session.ListResources(ctx, nil)
	if err != nil {
		log.Fatalf("list resources: %v", err)
	}
	fmt.Printf("\nresources (%d):\n", len(resources.Resources))
	for _, r := range resources.Resources {
		fmt.Printf("  %-32s %s\n", r.URI, r.Title)
	}

	templates, err := session.ListResourceTemplates(ctx, nil)
	if err == nil {
		fmt.Printf("resource templates (%d):\n", len(templates.ResourceTemplates))
		for _, t := range templates.ResourceTemplates {
			fmt.Printf("  %-32s %s\n", t.URITemplate, t.Title)
		}
	}

	// The format doc is what an authoring agent reads first.
	doc, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "deadlocker://docs/format"})
	if err != nil {
		log.Fatalf("read format doc: %v", err)
	}
	fmt.Printf("\nformat doc: %d bytes\n", len(doc.Contents[0].Text))

	// ------------------------------------------------------------ reading
	out := call(ctx, session, "list_scenarios", map[string]any{})
	var listed struct {
		Total int `json:"total"`
	}
	decode(out, &listed)
	fmt.Printf("\nlist_scenarios: %d scenarios\n", listed.Total)

	out = call(ctx, session, "get_scenario", map[string]any{"id": "uuidv7-missing-row-gap-lock"})
	var got struct {
		YAML string `json:"yaml"`
	}
	decode(out, &got)
	fmt.Printf("get_scenario: %d bytes of YAML\n", len(got.YAML))

	// --------------------------------------------------- authoring a case
	fmt.Println("\n--- authoring a scenario ---")

	broken := "name: missing pieces\n"
	out = call(ctx, session, "validate_scenario", map[string]any{"yaml": broken})
	var v struct {
		Valid bool   `json:"valid"`
		Error string `json:"error"`
	}
	decode(out, &v)
	fmt.Printf("validate (broken): valid=%v error=%q\n", v.Valid, v.Error)

	// A scenario that parses but wedges: the blocked actor gets another
	// statement before anything releases the lock.
	wedging := scenarioYAML(true)
	out = call(ctx, session, "validate_scenario", map[string]any{"yaml": wedging})
	var w struct {
		Valid    bool     `json:"valid"`
		Warnings []string `json:"warnings"`
	}
	decode(out, &w)
	fmt.Printf("validate (wedging): valid=%v warnings=%d\n", w.Valid, len(w.Warnings))
	for _, warn := range w.Warnings {
		fmt.Printf("   ! %s\n", warn)
	}

	good := scenarioYAML(false)
	out = call(ctx, session, "create_scenario", map[string]any{
		"yaml": good, "path": "mcp-probe/probe-gap-lock.yaml",
	})
	var created struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	decode(out, &created)
	fmt.Printf("create_scenario: id=%s path=%s\n", created.ID, created.Path)

	// --------------------------------------------------------- running it
	fmt.Println("\n--- running it ---")
	out = call(ctx, session, "start_run", map[string]any{"scenario_id": created.ID})
	var started struct {
		Run struct {
			RunID string `json:"run_id"`
			Total int    `json:"total"`
		} `json:"run"`
	}
	decode(out, &started)
	fmt.Printf("start_run: %s (%d steps)\n", started.Run.RunID, started.Run.Total)

	out = call(ctx, session, "step_run", map[string]any{
		"run_id": started.Run.RunID, "count": started.Run.Total,
	})
	var stepped struct {
		Outcomes []struct {
			Step struct {
				Index   int    `json:"index"`
				Actor   string `json:"actor"`
				Status  string `json:"status"`
				Verdict string `json:"verdict"`
			} `json:"step"`
			Locks struct {
				Cycle bool `json:"cycle"`
				Locks []struct {
					Actor   string `json:"actor"`
					Mode    string `json:"mode"`
					Status  string `json:"status"`
					Data    string `json:"data"`
					Explain string `json:"explain"`
				} `json:"locks"`
				Waits []struct {
					Waiting  string `json:"waiting_actor"`
					Blocking string `json:"blocking_actor"`
				} `json:"waits"`
			} `json:"locks"`
		} `json:"outcomes"`
		Stopped string `json:"stopped"`
	}
	decode(out, &stepped)

	fmt.Printf("step_run: %d step(s) submitted\n", len(stepped.Outcomes))
	for _, o := range stepped.Outcomes {
		fmt.Printf("  %2d %-3s %-8s %-8s", o.Step.Index, o.Step.Actor, o.Step.Status, o.Step.Verdict)
		if len(o.Locks.Waits) > 0 {
			fmt.Printf("  waits: ")
			for _, wt := range o.Locks.Waits {
				fmt.Printf("%s→%s ", wt.Waiting, wt.Blocking)
			}
		}
		fmt.Println()
	}
	if stepped.Stopped != "" {
		fmt.Printf("  stopped: %s\n", stepped.Stopped)
	}

	// The gap lock is the whole point; show that it reached the client.
	if n := len(stepped.Outcomes); n > 0 {
		for _, l := range stepped.Outcomes[n-1].Locks.Locks {
			if strings.Contains(l.Mode, "INSERT_INTENTION") || l.Data == "supremum pseudo-record" {
				fmt.Printf("  lock: %-6s %-20s %-8s %s\n", l.Actor, l.Mode, l.Status, l.Data)
			}
		}
	}

	out = call(ctx, session, "close_run", map[string]any{"run_id": started.Run.RunID})
	fmt.Printf("close_run: %s\n", firstLine(out))

	// ---------------------------------------------------------- updating
	fmt.Println("\n--- updating it ---")
	updated := strings.Replace(good, "REPEATABLE READ", "READ COMMITTED", 1)
	out = call(ctx, session, "update_scenario", map[string]any{"id": created.ID, "yaml": updated})
	var up struct {
		Scenario struct {
			Isolation string `json:"isolation"`
		} `json:"scenario"`
	}
	decode(out, &up)
	fmt.Printf("update_scenario: isolation is now %s\n", up.Scenario.Isolation)

	// Creating over an existing file must fail rather than silently overwrite.
	out = call(ctx, session, "create_scenario", map[string]any{
		"yaml": updated, "path": "mcp-probe/probe-gap-lock.yaml",
	})
	fmt.Printf("create over existing path: %s\n", firstLine(out))

	// And so must creating a scenario whose derived id is already taken.
	out = call(ctx, session, "create_scenario", map[string]any{
		"yaml": updated, "path": "elsewhere/probe-gap-lock.yaml",
	})
	fmt.Printf("create with a taken id:    %s\n", firstLine(out))

	out = call(ctx, session, "list_history", map[string]any{"limit": 3})
	fmt.Printf("\nlist_history: %s\n", firstLine(out))

	fmt.Println("\nprobe finished")
}

func call(ctx context.Context, s *mcp.ClientSession, name string, args map[string]any) string {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		log.Fatalf("%s: %v", name, err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return "ERROR: " + text
	}
	return text
}

func decode(payload string, into any) {
	if strings.HasPrefix(payload, "ERROR:") {
		log.Fatalf("tool returned an error: %s", payload)
	}
	if err := json.Unmarshal([]byte(payload), into); err != nil {
		log.Fatalf("decode %q: %v", firstLine(payload), err)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 150 {
		s = s[:149] + "…"
	}
	return s
}

// scenarioYAML returns the probe scenario. When wedge is true the ordering is
// deliberately wrong, so the linter has something to complain about.
func scenarioYAML(wedge bool) string {
	tail := `
  - actor: a
    label: Commit
    sql: COMMIT
    expect: ok

  - actor: b
    label: Commit
    sql: COMMIT
    expect: ok
`
	if wedge {
		tail = `
  - actor: b
    label: Another statement on the blocked connection
    sql: SELECT COUNT(*) FROM probe
    expect: ok

  - actor: a
    label: Commit
    sql: COMMIT
    expect: ok
`
	}
	return `name: MCP probe gap lock
category: MCP probe
description: |
  Written by the MCP probe to check the full authoring loop.
tags: [probe, gap-lock]

mysql:
  image: mysql:8.4
  isolation: REPEATABLE READ
  lock_wait_timeout: 60

schema:
  - |
    CREATE TABLE probe (
      id INT NOT NULL,
      val VARCHAR(32) NOT NULL,
      PRIMARY KEY (id)
    ) ENGINE=InnoDB

seed:
  - INSERT INTO probe (id, val) VALUES (10, 'ten'), (20, 'twenty')

actors:
  - id: a
    name: Session A
    accent: blue
  - id: b
    name: Session B
    accent: amber

steps:
  - actor: a
    label: Open a transaction
    sql: BEGIN
    expect: ok

  - actor: a
    label: Lock a gap that has no row
    sql: SELECT * FROM probe WHERE id = 15 FOR UPDATE
    expect: ok

  - actor: b
    label: Open a transaction
    sql: BEGIN
    expect: ok

  - actor: b
    label: Insert into the locked gap
    sql: INSERT INTO probe (id, val) VALUES (17, 'seventeen')
    expect: blocks
` + tail
}
