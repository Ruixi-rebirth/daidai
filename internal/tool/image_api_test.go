package tool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTextToImageAPI_GenerateFromURLAndBase64(t *testing.T) {
	samplePNG := tinyPNGBytes(t)

	t.Run("URLResponse", func(t *testing.T) {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/images/generations":
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("unexpected auth header: %q", r.Header.Get("Authorization"))
				}
				var req map[string]any
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req["model"] != "cogview-3-flash" {
					t.Errorf("unexpected model: %#v", req["model"])
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": []map[string]string{{"url": srv.URL + "/img.png"}},
				})
			case "/img.png":
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(samplePNG)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		api := NewTextToImageAPI(srv.URL, "test-key", "cogview-3-flash")
		api.HTTPClient = srv.Client()
		data, err := api.Generate(context.Background(), "一只晒太阳的橘猫")
		if err != nil {
			t.Fatalf("Generate(URL) failed: %v", err)
		}
		if len(data) == 0 {
			t.Fatal("expected non-empty normalized PNG")
		}
	})

	t.Run("Base64Response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(samplePNG)}},
			})
		}))
		defer srv.Close()

		api := NewTextToImageAPI(srv.URL, "test-key", "cogview-3-flash")
		api.HTTPClient = srv.Client()
		data, err := api.Generate(context.Background(), "一只橘猫")
		if err != nil {
			t.Fatalf("Generate(b64_json) failed: %v", err)
		}
		if len(data) == 0 {
			t.Fatal("expected non-empty normalized PNG")
		}
	})
}
