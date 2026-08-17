// Command stopprobe checks that stopping a run from the UI reaches an agent
// that is stepping it.
//
// The point of the stop button is not just to halt the run: the model has to
// find out that a person intervened, otherwise it just retries. This drives the
// real MCP tool and asserts the interruption comes back in its result.
//
//	go run ./hack/stopprobe [base-url]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	base := "http://127.0.0.1:8899"
	if len(os.Args) > 1 {
		base = strings.TrimRight(os.Args[1], "/")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "stopprobe", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer session.Close()

	// Start a run through MCP, exactly as the assistant would.
	var started struct {
		Run struct {
			RunID string `json:"run_id"`
			Total int    `json:"total"`
		} `json:"run"`
	}
	decode(call(ctx, session, "start_run", map[string]any{"scenario_id": "record-lock-basics"}), &started)
	run := started.Run.RunID
	fmt.Printf("started run %s (%d steps)\n", run, started.Run.Total)

	// One normal step, to show the difference.
	var first struct {
		Outcomes []any  `json:"outcomes"`
		Stopped  string `json:"stopped"`
	}
	decode(call(ctx, session, "step_run", map[string]any{"run_id": run}), &first)
	fmt.Printf("step before the stop: %d outcome(s), stopped=%q\n", len(first.Outcomes), first.Stopped)
	if len(first.Outcomes) != 1 {
		log.Fatalf("expected one step to run, got %d", len(first.Outcomes))
	}

	// A person presses Stop in the UI.
	post(base + "/run/" + run + "/stop")
	fmt.Println("pressed Stop in the UI")

	// The agent's next step must come back explaining itself, having run nothing.
	var after struct {
		Outcomes []any  `json:"outcomes"`
		Stopped  string `json:"stopped"`
	}
	decode(call(ctx, session, "step_run", map[string]any{"run_id": run, "count": 5}), &after)
	fmt.Printf("step after the stop:  %d outcome(s), stopped=%q\n", len(after.Outcomes), after.Stopped)

	if len(after.Outcomes) != 0 {
		log.Fatalf("FAIL: the run advanced %d step(s) after being stopped", len(after.Outcomes))
	}
	if !strings.Contains(strings.ToLower(after.Stopped), "user") {
		log.Fatalf("FAIL: the agent was not told a person intervened: %q", after.Stopped)
	}

	// The stop is consumed, so the run can continue if the user asks for it.
	var resumed struct {
		Outcomes []any  `json:"outcomes"`
		Stopped  string `json:"stopped"`
	}
	decode(call(ctx, session, "step_run", map[string]any{"run_id": run}), &resumed)
	fmt.Printf("step after that:      %d outcome(s), stopped=%q\n", len(resumed.Outcomes), resumed.Stopped)
	if len(resumed.Outcomes) != 1 {
		log.Fatalf("FAIL: the run stayed stuck after the interruption was reported")
	}

	call(ctx, session, "close_run", map[string]any{"run_id": run})
	fmt.Println("\nok: the stop halted the run, told the agent why, and did not wedge it")
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
		log.Fatalf("%s returned an error: %s", name, text)
	}
	return text
}

func decode(payload string, into any) {
	if err := json.Unmarshal([]byte(payload), into); err != nil {
		log.Fatalf("decode: %v (payload: %.200s)", err, payload)
	}
}

func post(url string) {
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		log.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Fatalf("POST %s: %s", url, resp.Status)
	}
}
