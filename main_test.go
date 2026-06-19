package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
)

func setupTestApp(t *testing.T, configContent string, apiKeys []string, adminKey string) (*fiber.App, func()) {
	tmpFile, err := os.CreateTemp("", "test_config_*.json")
	if err != nil {
		t.Fatalf("failed to create temp config file: %v", err)
	}
	defer tmpFile.Close()

	if _, err := tmpFile.WriteString(configContent); err != nil {
		t.Fatalf("failed to write to temp config file: %v", err)
	}

	modelsMap = make(map[string]ModelConfig)
	modelsList = nil

	if err := loadConfig(tmpFile.Name()); err != nil {
		t.Fatalf("failed to load config in test: %v", err)
	}

	// Initialize Database in-memory for testing
	if err := initDB(":memory:"); err != nil {
		t.Fatalf("failed to initialize in-memory SQLite database: %v", err)
	}

	app := fiber.New()
	app.Use(cors.New())

	// Auth Middleware (only mount if static keys exist or admin key is configured for dynamic keys)
	keysMap := make(map[string]bool)
	for _, k := range apiKeys {
		keysMap[k] = true
	}
	if len(keysMap) > 0 || adminKey != "" {
		app.Use("/v1", authMiddleware(keysMap))
	}

	// Admin routes
	if adminKey != "" {
		adminGroup := app.Group("/admin", adminAuthMiddleware(adminKey))
		adminGroup.Get("/keys", handleListKeys)
		adminGroup.Post("/keys", handleCreateKey)
		adminGroup.Delete("/keys/:key", handleRevokeKey)
	}

	app.Get("/v1/models", handleGetModels)
	app.Post("/v1/chat/completions", handleProxyRequest)
	app.Post("/v1/completions", handleProxyRequest)
	app.Post("/v1/embeddings", handleProxyRequest)

	cleanup := func() {
		os.Remove(tmpFile.Name())
		if db != nil {
			db.Close()
			db = nil
		}
	}

	return app, cleanup
}

func TestGetModels(t *testing.T) {
	configJSON := `[
		{"model": "qwen-test", "port": 12345, "host": "127.0.0.1", "owned_by": "custom-owner"},
		{"model": "gemma-test", "port": 54321, "owner": "custom-owner-2"}
	]`
	app, cleanup := setupTestApp(t, configJSON, nil, "")
	defer cleanup()

	req, err := http.NewRequest("GET", "/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var modelsResp ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if modelsResp.Object != "list" {
		t.Errorf("expected object list, got %s", modelsResp.Object)
	}

	if len(modelsResp.Data) != 2 {
		t.Fatalf("expected 2 models, got %d", len(modelsResp.Data))
	}

	if modelsResp.Data[0].ID != "qwen-test" || modelsResp.Data[0].OwnedBy != "custom-owner" {
		t.Errorf("model 1 details mismatch: %+v", modelsResp.Data[0])
	}

	if modelsResp.Data[1].ID != "gemma-test" || modelsResp.Data[1].OwnedBy != "custom-owner-2" {
		t.Errorf("model 2 details mismatch: %+v", modelsResp.Data[1])
	}
}

func TestProxyErrors(t *testing.T) {
	configJSON := `[
		{"model": "offline-model", "port": 50099, "host": "127.0.0.1"}
	]`
	app, cleanup := setupTestApp(t, configJSON, nil, "")
	defer cleanup()

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "Missing model field",
			method:     "POST",
			path:       "/v1/chat/completions",
			body:       `{"messages": []}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "missing_model",
		},
		{
			name:       "Invalid JSON body",
			method:     "POST",
			path:       "/v1/chat/completions",
			body:       `{"messages":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_json",
		},
		{
			name:       "Unknown model",
			method:     "POST",
			path:       "/v1/chat/completions",
			body:       `{"model": "unknown-model"}`,
			wantStatus: http.StatusNotFound,
			wantCode:   "model_not_found",
		},
		{
			name:       "Offline upstream server",
			method:     "POST",
			path:       "/v1/chat/completions",
			body:       `{"model": "offline-model"}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "bad_gateway",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("expected status %d, got %d", tt.wantStatus, resp.StatusCode)
			}

			var errResp OpenAIErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
				t.Fatalf("failed to decode error response: %v", err)
			}

			if errResp.Error.Code == nil || *errResp.Error.Code != tt.wantCode {
				t.Errorf("expected error code %s, got %v", tt.wantCode, errResp.Error.Code)
			}
		})
	}
}

func TestProxyStreamingAndStandard(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)

			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", 500)
				return
			}

			for i := 1; i <= 3; i++ {
				fmt.Fprintf(w, "data: chunk-%d\n\n", i)
				flusher.Flush()
				time.Sleep(10 * time.Millisecond)
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices": [{"text": "standard-response"}]}`))
		}
	}))
	defer mockServer.Close()

	var mockHost string
	var mockPort int
	_, err := fmt.Sscanf(strings.TrimPrefix(mockServer.URL, "http://"), "%s:%d", &mockHost, &mockPort)
	if err != nil {
		parts := strings.Split(strings.TrimPrefix(mockServer.URL, "http://"), ":")
		if len(parts) == 2 {
			mockHost = parts[0]
			fmt.Sscanf(parts[1], "%d", &mockPort)
		}
	}
	if mockHost == "" {
		mockHost = "127.0.0.1"
	}

	configJSON := fmt.Sprintf(`[
		{"model": "mock-model", "port": %d, "host": "%s"}
	]`, mockPort, mockHost)

	app, cleanup := setupTestApp(t, configJSON, nil, "")
	defer cleanup()

	t.Run("Standard request forwarding", func(t *testing.T) {
		req, err := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model": "mock-model", "stream": false}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		expected := `{"choices": [{"text": "standard-response"}]}`
		if string(bodyBytes) != expected {
			t.Errorf("expected response %s, got %s", expected, string(bodyBytes))
		}
	})

	t.Run("Streaming request forwarding", func(t *testing.T) {
		req, err := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model": "mock-model", "stream": true}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}

		if resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Errorf("expected Content-Type text/event-stream, got %s", resp.Header.Get("Content-Type"))
		}

		scanner := bufio.NewScanner(resp.Body)
		var chunks []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				chunks = append(chunks, line)
			}
		}

		if len(chunks) != 3 {
			t.Fatalf("expected 3 chunks, got %d: %+v", len(chunks), chunks)
		}

		if chunks[0] != "data: chunk-1" || chunks[1] != "data: chunk-2" || chunks[2] != "data: chunk-3" {
			t.Errorf("unexpected chunks sequence: %+v", chunks)
		}
	})
}

func TestAuthMiddleware(t *testing.T) {
	configJSON := `[
		{"model": "qwen-test", "port": 12345}
	]`
	app, cleanup := setupTestApp(t, configJSON, []string{"key-1", "key-2"}, "")
	defer cleanup()

	tests := []struct {
		name       string
		header     string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "Missing auth header",
			header:     "",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "missing_api_key",
		},
		{
			name:       "Incorrect prefix",
			header:     "key-1",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "invalid_api_key",
		},
		{
			name:       "Incorrect key",
			header:     "Bearer wrong-key",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "invalid_api_key",
		},
		{
			name:       "Correct key-1 but offline target",
			header:     "Bearer key-1",
			wantStatus: http.StatusBadGateway,
			wantCode:   "bad_gateway",
		},
		{
			name:       "Correct key-2 but offline target",
			header:     "Bearer key-2",
			wantStatus: http.StatusBadGateway,
			wantCode:   "bad_gateway",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model": "qwen-test"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("expected status %d, got %d", tt.wantStatus, resp.StatusCode)
			}

			var errResp OpenAIErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
				t.Fatalf("failed to decode error response: %v", err)
			}

			if errResp.Error.Code == nil || *errResp.Error.Code != tt.wantCode {
				t.Errorf("expected error code %s, got %v", tt.wantCode, errResp.Error.Code)
			}
		})
	}
}

func TestAdminEndpoints(t *testing.T) {
	configJSON := `[
		{"model": "qwen-test", "port": 12345}
	]`
	app, cleanup := setupTestApp(t, configJSON, nil, "admin-master")
	defer cleanup()

	// 1. Verify list is empty initially
	t.Run("List keys initially empty", func(t *testing.T) {
		req, err := http.NewRequest("GET", "/admin/keys", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer admin-master")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}

		var keys []KeyInfo
		if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
			t.Fatal(err)
		}
		if len(keys) != 0 {
			t.Errorf("expected 0 keys, got %d", len(keys))
		}
	})

	// 2. Verify unauthorized access to admin endpoints
	t.Run("Unauthorized list keys", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/admin/keys", nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", resp.StatusCode)
		}
	})

	var createdKey string

	// 3. Create a key with a name and a tag
	t.Run("Create API key with tag and name", func(t *testing.T) {
		body := `{"name": "test-user", "tag": "prod"}`
		req, _ := http.NewRequest("POST", "/admin/keys", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-master")
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			t.Errorf("expected 201, got %d", resp.StatusCode)
		}

		var keyObj KeyInfo
		if err := json.NewDecoder(resp.Body).Decode(&keyObj); err != nil {
			t.Fatal(err)
		}

		if keyObj.Name != "test-user" || keyObj.Tag != "prod" {
			t.Errorf("expected name 'test-user' and tag 'prod', got Name='%s' Tag='%s'", keyObj.Name, keyObj.Tag)
		}
		if !strings.HasPrefix(keyObj.Key, "flm_") {
			t.Errorf("expected key to start with 'flm_', got %s", keyObj.Key)
		}
		createdKey = keyObj.Key
	})

	// 4. Verify the newly created key can authenticate chat/completions requests
	t.Run("Authenticate v1 request using dynamic key", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model": "qwen-test"}`))
		req.Header.Set("Authorization", "Bearer "+createdKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("expected 502 Bad Gateway (auth passed, routing failed), got %d", resp.StatusCode)
		}
	})

	// 5. Revoke/delete the key
	t.Run("Revoke API key", func(t *testing.T) {
		req, _ := http.NewRequest("DELETE", "/admin/keys/"+createdKey, nil)
		req.Header.Set("Authorization", "Bearer admin-master")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
	})

	// 6. Verify the revoked key can no longer authenticate requests
	t.Run("Authenticate using revoked key fails", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model": "qwen-test"}`))
		req.Header.Set("Authorization", "Bearer "+createdKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
		}
	})
}
