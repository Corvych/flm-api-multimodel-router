package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/valyala/fasthttp"

	_ "modernc.org/sqlite"
)

// ModelConfig holds the routing target configuration for a single model.
type ModelConfig struct {
	Model   string `json:"model"`
	Port    int    `json:"port"`
	Host    string `json:"host,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`
	Owner   string `json:"owner,omitempty"` // Alias for OwnedBy
}

// ModelRequest represents the minimum JSON structure to extract the model parameter.
type ModelRequest struct {
	Model string `json:"model"`
}

// OpenAIErrorDetails holds the specific fields of the OpenAI error schema.
type OpenAIErrorDetails struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// OpenAIErrorResponse is the top-level OpenAI error response envelope.
type OpenAIErrorResponse struct {
	Error OpenAIErrorDetails `json:"error"`
}

// ModelsResponse matches the GET /v1/models response envelope.
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// ModelInfo is the structure returned for each model in /v1/models list.
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

var (
	modelsMap  = make(map[string]ModelConfig)
	modelsList []ModelConfig
	startTime  = time.Now().Unix()
	db         *sql.DB
)

// KeyInfo holds metadata for API keys in the SQLite database.
type KeyInfo struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Tag       string    `json:"tag"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateKeyRequest represents the POST payload to register a new API key.
type CreateKeyRequest struct {
	Key  string `json:"key,omitempty"`
	Name string `json:"name"`
	Tag  string `json:"tag,omitempty"`
}

// Custom HTTP Client tuned for proxying long-running LLM inference connections.
var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        500,
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func sendOpenAIError(c fiber.Ctx, statusCode int, message, errType, code string) error {
	var codePtr *string
	if code != "" {
		codePtr = &code
	}
	resp := OpenAIErrorResponse{
		Error: OpenAIErrorDetails{
			Message: message,
			Type:    errType,
			Param:   nil,
			Code:    codePtr,
		},
	}
	return c.Status(statusCode).JSON(resp)
}

func authMiddleware(apiKeys map[string]bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		authHeader := c.Get("Authorization")
		if authHeader == "" {
			return sendOpenAIError(c, fiber.StatusUnauthorized, "Missing Authorization header", "invalid_request_error", "missing_api_key")
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == authHeader {
			return sendOpenAIError(c, fiber.StatusUnauthorized, "Incorrect API key provided", "invalid_request_error", "invalid_api_key")
		}

		// First, check static config keys
		if apiKeys[token] {
			return c.Next()
		}

		// Second, check dynamically registered SQLite keys
		if db != nil {
			var exists bool
			err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM api_keys WHERE key = ?)", token).Scan(&exists)
			if err == nil && exists {
				return c.Next()
			}
		}

		return sendOpenAIError(c, fiber.StatusUnauthorized, "Incorrect API key provided", "invalid_request_error", "invalid_api_key")
	}
}

func adminAuthMiddleware(adminKey string) fiber.Handler {
	return func(c fiber.Ctx) error {
		authHeader := c.Get("Authorization")
		if authHeader == "" {
			return sendOpenAIError(c, fiber.StatusUnauthorized, "Missing Authorization header", "invalid_request_error", "missing_admin_key")
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == authHeader || token != adminKey {
			return sendOpenAIError(c, fiber.StatusUnauthorized, "Incorrect admin API key provided", "invalid_request_error", "invalid_admin_key")
		}

		return c.Next()
	}
}

func initDB(dbPath string) error {
	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Restrict SQLite to a single writer connection to avoid lock contention
	db.SetMaxOpenConns(1)

	schema := `
	CREATE TABLE IF NOT EXISTS api_keys (
		key TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		tag TEXT NOT NULL,
		created_at DATETIME NOT NULL
	);`

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("failed to initialize schema: %w", err)
	}

	return nil
}

func generateRandomKey() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "flm_" + hex.EncodeToString(bytes), nil
}

func handleListKeys(c fiber.Ctx) error {
	rows, err := db.Query("SELECT key, name, tag, created_at FROM api_keys ORDER BY created_at DESC")
	if err != nil {
		return sendOpenAIError(c, fiber.StatusInternalServerError, "Failed to query keys database", "internal_error", "")
	}
	defer rows.Close()

	var keys []KeyInfo
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.Key, &k.Name, &k.Tag, &k.CreatedAt); err != nil {
			return sendOpenAIError(c, fiber.StatusInternalServerError, "Failed to scan key record", "internal_error", "")
		}
		keys = append(keys, k)
	}
	if keys == nil {
		keys = []KeyInfo{}
	}
	return c.JSON(keys)
}

func handleCreateKey(c fiber.Ctx) error {
	var req CreateKeyRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return sendOpenAIError(c, fiber.StatusBadRequest, "Invalid JSON body", "invalid_request_error", "invalid_json")
	}

	key := strings.TrimSpace(req.Key)
	name := strings.TrimSpace(req.Name)
	tag := strings.TrimSpace(req.Tag)

	if name == "" {
		return sendOpenAIError(c, fiber.StatusBadRequest, "Name field is required", "invalid_request_error", "missing_name")
	}

	if key == "" {
		var err error
		key, err = generateRandomKey()
		if err != nil {
			return sendOpenAIError(c, fiber.StatusInternalServerError, "Failed to generate random API key", "internal_error", "")
		}
	}

	now := time.Now().UTC()
	_, err := db.Exec("INSERT INTO api_keys (key, name, tag, created_at) VALUES (?, ?, ?, ?)", key, name, tag, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return sendOpenAIError(c, fiber.StatusConflict, "API key already exists", "invalid_request_error", "duplicate_key")
		}
		return sendOpenAIError(c, fiber.StatusInternalServerError, "Failed to save API key to database", "internal_error", "")
	}

	resp := KeyInfo{
		Key:       key,
		Name:      name,
		Tag:       tag,
		CreatedAt: now,
	}
	return c.Status(fiber.StatusCreated).JSON(resp)
}

func handleRevokeKey(c fiber.Ctx) error {
	key := c.Params("key")
	if key == "" {
		return sendOpenAIError(c, fiber.StatusBadRequest, "Key parameter is required", "invalid_request_error", "missing_key")
	}

	result, err := db.Exec("DELETE FROM api_keys WHERE key = ?", key)
	if err != nil {
		return sendOpenAIError(c, fiber.StatusInternalServerError, "Failed to delete API key from database", "internal_error", "")
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return sendOpenAIError(c, fiber.StatusInternalServerError, "Failed to check delete query status", "internal_error", "")
	}

	if rowsAffected == 0 {
		return sendOpenAIError(c, fiber.StatusNotFound, "API key not found", "invalid_request_error", "key_not_found")
	}

	return c.JSON(fiber.Map{"status": "revoked"})
}

func main() {
	// Parse CLI options with environment variable fallbacks
	defaultConfigPath := getEnv("ROUTER_CONFIG_PATH", "config.json")
	defaultPort := getEnv("ROUTER_PORT", "8080")
	defaultHost := getEnv("ROUTER_HOST", "0.0.0.0")
	defaultDBPath := getEnv("ROUTER_DB_PATH", "keys.db")
	defaultAdminKey := getEnv("ROUTER_ADMIN_KEY", "")
	
	defaultAPIKeys := getEnv("ROUTER_API_KEYS", "")
	if defaultAPIKeys == "" {
		defaultAPIKeys = getEnv("ROUTER_API_KEY", "")
	}

	configPath := flag.String("config", defaultConfigPath, "Path to the config.json file")
	port := flag.String("port", defaultPort, "Port to run the router on")
	host := flag.String("host", defaultHost, "Host IP to bind to")
	apiKeys := flag.String("api-keys", defaultAPIKeys, "Comma-separated list of API keys required to access the router (leave empty to disable)")
	dbPath := flag.String("db-path", defaultDBPath, "Path to the SQLite database file")
	adminKey := flag.String("admin-key", defaultAdminKey, "Admin master key to access key management endpoints (disabled if empty)")
	flag.Parse()

	apiKeysMap := make(map[string]bool)
	if *apiKeys != "" {
		for _, k := range strings.Split(*apiKeys, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				apiKeysMap[k] = true
			}
		}
	}

	log.Printf("Starting FastFlowLM Router...")
	log.Printf("Loading configuration from: %s", *configPath)

	// Load and parse configuration
	if err := loadConfig(*configPath); err != nil {
		log.Fatalf("Fatal: Failed to load config: %v", err)
	}

	// Initialize SQLite Database
	log.Printf("Initializing database at: %s", *dbPath)
	if err := initDB(*dbPath); err != nil {
		log.Fatalf("Fatal: Failed to initialize database: %v", err)
	}
	defer db.Close()

	// Initialize Fiber v3
	app := fiber.New(fiber.Config{
		AppName:               "FastFlowLM Multimodel Router v3",
	})

	// Middleware
	app.Use(cors.New())
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${status} - ${latency} ${method} ${path}\n",
	}))

	// Auth Middleware (only mount if static keys exist or admin key is configured for dynamic keys)
	if len(apiKeysMap) > 0 || *adminKey != "" {
		app.Use("/v1", authMiddleware(apiKeysMap))
	}

	// Admin Routes (only registered if adminKey is configured)
	if *adminKey != "" {
		adminGroup := app.Group("/admin", adminAuthMiddleware(*adminKey))
		adminGroup.Get("/keys", handleListKeys)
		adminGroup.Post("/keys", handleCreateKey)
		adminGroup.Delete("/keys/:key", handleRevokeKey)
		log.Println("Admin key management endpoints registered at /admin/keys")
	} else {
		log.Println("Admin key management endpoints are disabled (ROUTER_ADMIN_KEY/admin-key not set)")
	}

	// API Routes
	app.Get("/v1/models", handleGetModels)
	app.Post("/v1/chat/completions", handleProxyRequest)
	app.Post("/v1/completions", handleProxyRequest)
	app.Post("/v1/embeddings", handleProxyRequest)

	// Start server in a goroutine
	addr := fmt.Sprintf("%s:%s", *host, *port)
	go func() {
		log.Printf("Router is listening on http://%s", addr)
		if err := app.Listen(addr); err != nil {
			log.Fatalf("Fatal: Server error: %v", err)
		}
	}()

	// Graceful shutdown handling
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down router...")
	if err := app.Shutdown(); err != nil {
		log.Printf("Error during server shutdown: %v", err)
	}
	log.Println("Router stopped.")
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func loadConfig(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	var configs []ModelConfig
	if err := json.NewDecoder(file).Decode(&configs); err != nil {
		return err
	}

	for _, cfg := range configs {
		if cfg.Model == "" {
			return fmt.Errorf("invalid config: model name cannot be empty")
		}
		if cfg.Port <= 0 || cfg.Port > 65535 {
			return fmt.Errorf("invalid config: port %d for model %s is invalid", cfg.Port, cfg.Model)
		}
		// Support both owner and owned_by aliases
		if cfg.OwnedBy == "" && cfg.Owner != "" {
			cfg.OwnedBy = cfg.Owner
		}
		modelsMap[cfg.Model] = cfg
		modelsList = append(modelsList, cfg)
		log.Printf("Registered model routing: %s -> %s:%d", cfg.Model, cfg.Host, cfg.Port)
	}

	return nil
}

func handleGetModels(c fiber.Ctx) error {
	data := make([]ModelInfo, 0, len(modelsList))
	for _, m := range modelsList {
		ownedBy := m.OwnedBy
		if ownedBy == "" {
			ownedBy = "fastflowlm"
		}
		data = append(data, ModelInfo{
			ID:      m.Model,
			Object:  "model",
			Created: startTime,
			OwnedBy: ownedBy,
		})
	}
	return c.JSON(ModelsResponse{
		Object: "list",
		Data:   data,
	})
}

func handleProxyRequest(c fiber.Ctx) error {
	body := c.Body()

	var modelReq ModelRequest
	if err := json.Unmarshal(body, &modelReq); err != nil {
		return sendOpenAIError(c, fiber.StatusBadRequest, "Invalid request payload: must be valid JSON", "invalid_request_error", "invalid_json")
	}

	if modelReq.Model == "" {
		return sendOpenAIError(c, fiber.StatusBadRequest, "Missing 'model' field in request body", "invalid_request_error", "missing_model")
	}

	modelCfg, ok := modelsMap[modelReq.Model]
	if !ok {
		return sendOpenAIError(c, fiber.StatusNotFound, fmt.Sprintf("Model '%s' not found in routing configuration", modelReq.Model), "invalid_request_error", "model_not_found")
	}

	host := modelCfg.Host
	if host == "" {
		host = "localhost"
	}

	upstreamURL := fmt.Sprintf("http://%s:%d%s", host, modelCfg.Port, c.Path())

	// Forward client request
	upstreamReq, err := http.NewRequest(c.Method(), upstreamURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("Error creating upstream request: %v", err)
		return sendOpenAIError(c, fiber.StatusInternalServerError, "Failed to create upstream request", "api_error", "internal_error")
	}

	// Copy incoming request headers to upstream
	c.Request().Header.VisitAll(func(key, val []byte) {
		k := string(key)
		v := string(val)
		// Skip Connection and Host headers to let the HTTP client negotiate them
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Connection") {
			return
		}
		upstreamReq.Header.Add(k, v)
	})

	// Execute HTTP request
	resp, err := httpClient.Do(upstreamReq)
	if err != nil {
		log.Printf("Upstream error forwarding model request for '%s': %v", modelReq.Model, err)
		return sendOpenAIError(c, fiber.StatusBadGateway, fmt.Sprintf("Upstream model server for '%s' is unreachable", modelReq.Model), "api_error", "bad_gateway")
	}

	// Copy headers from upstream response to client response
	for k, vv := range resp.Header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Connection") {
			continue
		}
		for _, v := range vv {
			c.Set(k, v)
		}
	}
	c.Status(resp.StatusCode)

	// Stream response back if Content-Type is text/event-stream (SSE)
	isSSE := resp.Header.Get("Content-Type") == "text/event-stream"
	if isSSE {
		c.RequestCtx().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
			defer resp.Body.Close()
			buf := make([]byte, 4096)
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					if _, writeErr := w.Write(buf[:n]); writeErr != nil {
						// Client disconnected (detected during write failure)
						return
					}
					if flushErr := w.Flush(); flushErr != nil {
						// Client disconnected (detected during flush failure)
						return
					}
				}
				if err != nil {
					// End of stream or stream read error
					return
				}
			}
		}))
		return nil
	}

	// Non-streaming response
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading upstream response: %v", err)
		return sendOpenAIError(c, fiber.StatusInternalServerError, "Failed to read response from upstream", "api_error", "internal_error")
	}

	return c.Send(respBytes)
}
