package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Model is one entry from an OpenAI-compatible /models listing.
type Model struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// FetchModels asks an OpenAI-compatible endpoint what it can serve, so the
// settings UI can offer a dropdown instead of a free-text field where a typo
// fails silently at the first message.
func FetchModels(ctx context.Context, baseURL, apiKey string) ([]Model, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("no base URL configured")
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s/models: %w", base, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s/models returned %s: %s", base, resp.Status, snippet(body))
	}

	var payload struct {
		Data []Model `json:"data"`
		// Ollama's native endpoint uses a different shape; accept it too so a
		// base URL that points at the wrong path still gives a useful answer.
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("unexpected response from %s/models: %s", base, snippet(body))
	}

	models := payload.Data
	for _, m := range payload.Models {
		if m.Name != "" {
			models = append(models, Model{ID: m.Name})
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%s/models returned no models", base)
	}

	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:199] + "…"
	}
	return s
}
