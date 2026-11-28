package assistant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// M20I: an empty OpenAI-compatible list falls back to Gemini's native model list.
func TestGeminiModelsFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1beta/openai/models":
			w.Write([]byte(`{"data":[]}`))
		case "/v1beta/models":
			if r.Header.Get("x-goog-api-key") != "k" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":{"message":"API key not valid. Please pass a valid API key."}}`))
				return
			}
			w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]},{"name":"models/embedding-001","supportedGenerationMethods":["embedContent"]}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := Config{Provider: ProviderGemini, BaseURL: srv.URL + "/v1beta/openai", APIKey: "k"}
	ms, err := ListModels(context.Background(), c)
	if err != nil || len(ms) != 1 || ms[0].ID != "gemini-2.5-pro" {
		t.Fatalf("ListModels = %v, %v", ms, err)
	}
	c.APIKey = "bad"
	_, err = ListModels(context.Background(), c)
	if code, _ := ErrorCode(err); code != CodeAuth {
		t.Fatalf("bad key: code %q, err %v", code, err)
	}
}
