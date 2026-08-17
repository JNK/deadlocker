// Command fakellm is a stub OpenAI-compatible server for exercising the chat
// path without downloading a real model.
//
// It responds deterministically: on the first turn it calls a tool (set_draft
// when the request offers it, otherwise list_scenarios), and once it sees a
// tool result it replies with text. That is enough to drive provider setup,
// tool schema generation, tool execution and the streaming callbacks all the
// way to the browser.
//
//	go run ./hack/fakellm -addr 127.0.0.1:11500
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role string `json:"role"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:11500", "address to listen on")
	dump := flag.String("dump", "", "write each raw request body to this file, one JSON object per line")
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "stub-small", "object": "model", "owned_by": "fakellm"},
				{"id": "stub-large", "object": "model", "owned_by": "fakellm"},
			},
		})
	})

	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The raw body is what makes the sampling settings testable: they are
		// injected into it directly, so decoding into a struct would hide them.
		if *dump != "" {
			if f, err := os.OpenFile(*dump, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				fmt.Fprintf(f, "%s\n", body)
				f.Close()
			}
		}

		var req chatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		sawToolResult := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				sawToolResult = true
			}
		}
		available := map[string]bool{}
		for _, t := range req.Tools {
			available[t.Function.Name] = true
		}

		log.Printf("chat: model=%s messages=%d tools=%d toolResultSeen=%v",
			req.Model, len(req.Messages), len(req.Tools), sawToolResult)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		send := func(delta map[string]any, finish any) {
			chunk := map[string]any{
				"id": "chatcmpl-stub", "object": "chat.completion.chunk",
				"created": 0, "model": req.Model,
				"choices": []map[string]any{{
					"index": 0, "delta": delta, "finish_reason": finish,
				}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}

		if !sawToolResult {
			name, args := "list_scenarios", "{}"
			if available["set_draft"] {
				name, args = "set_draft", mustJSON(map[string]string{"yaml": draftYAML})
			}
			send(map[string]any{"role": "assistant"}, nil)
			send(map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_stub_1", "type": "function",
				"function": map[string]any{"name": name, "arguments": ""},
			}}}, nil)
			send(map[string]any{"tool_calls": []map[string]any{{
				"index":    0,
				"function": map[string]any{"arguments": args},
			}}}, nil)
			send(map[string]any{}, "tool_calls")
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		reply := "Done. I called a tool and read the result."
		if available["set_draft"] {
			reply = "I drafted a gap-lock scenario and put it in the draft pane. Run it to confirm step 4 blocks."
		}
		send(map[string]any{"role": "assistant"}, nil)
		for _, word := range strings.SplitAfter(reply, " ") {
			send(map[string]any{"content": word}, nil)
		}
		send(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})

	log.Printf("fake OpenAI-compatible server on http://%s/v1", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

const draftYAML = `name: Stub drafted gap lock
category: Stub
description: |
  Drafted by the stub model to exercise the builder flow.

mysql:
  image: mysql:8.4
  isolation: REPEATABLE READ
  lock_wait_timeout: 60

schema:
  - |
    CREATE TABLE stub (
      id INT NOT NULL,
      val VARCHAR(32) NOT NULL,
      PRIMARY KEY (id)
    ) ENGINE=InnoDB

seed:
  - INSERT INTO stub (id, val) VALUES (10, 'ten'), (20, 'twenty')

actors:
  - id: a
    name: Session A
    accent: blue
  - id: b
    name: Session B
    accent: amber

steps:
  - actor: a
    sql: BEGIN
    expect: ok
  - actor: a
    label: Lock an empty gap
    sql: SELECT * FROM stub WHERE id = 15 FOR UPDATE
    expect: ok
  - actor: b
    sql: BEGIN
    expect: ok
  - actor: b
    label: Insert into the gap
    sql: INSERT INTO stub (id, val) VALUES (17, 'seventeen')
    expect: blocks
  - actor: a
    sql: COMMIT
    expect: ok
  - actor: b
    sql: COMMIT
    expect: ok
`
