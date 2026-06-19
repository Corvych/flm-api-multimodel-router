# FastFlowLM Multimodel Router (Fiber v3)

A high-performance, lightweight multimodel router written in Go using **Fiber v3**. It routes incoming OpenAI-compatible requests (`/v1/chat/completions`, `/v1/completions`, and `/v1/embeddings`) to the appropriate FastFlowLM backend server based on a JSON routing configuration array. It fully supports Server-Sent Events (SSE) token streaming.

## Features

- **OpenAI-Compatible Structure:** Exposes `/v1/models` and handles standard completion endpoints.
- **Dynamic Routing:** Routes requests dynamically based on the `"model"` field in the JSON request body.
- **SSE Stream Proxying:** Forwards token-by-token streaming responses to clients in real-time without buffering.
- **Connection Pooling & Reuse:** Uses an optimized HTTP transport connection pool to minimize latency.
- **Robust Error Handling:** Returns proper OpenAI-formatted error envelopes when backends are unreachable or configurations are invalid.
- **Docker Ready:** Built-in multi-stage `Dockerfile` and `docker-compose.yml` for instant container deployment.

---

## Configuration (`config.json`)

Configure your model backends by defining them in a JSON array. Create a `config.json` in the root of the project:

```json
[
  {
    "model": "qwen2.5-7b",
    "port": 52625,
    "host": "localhost",
    "owned_by": "fastflowlm"
  },
  {
    "model": "gemma2-9b",
    "port": 52626,
    "host": "127.0.0.1",
    "owned_by": "custom-org"
  }
]
```

### Fields:
- `model` (Required): The identifier sent by the client in the `"model"` JSON parameter.
- `port` (Required): The port on which the corresponding FastFlowLM backend is listening.
- `host` (Optional): The hostname of the model server (defaults to `localhost`).
- `owned_by` (Optional): Metadata for `/v1/models` endpoint (defaults to `fastflowlm`).

---

## How to Run

### Locally

Ensure you have **Go 1.25+** installed.

1. **Install dependencies:**
   ```bash
   go mod tidy
   ```

2. **Run the server:**
   ```bash
   go run main.go -config config.json -port 8080 -host 0.0.0.0
   ```

#### CLI Flags and Environment Variables:
| Flag | Env Variable | Default | Description |
| :--- | :--- | :--- | :--- |
| `-config` | `ROUTER_CONFIG_PATH` | `config.json` | Path to the config file |
| `-port` | `ROUTER_PORT` | `8080` | Router listening port |
| `-host` | `ROUTER_HOST` | `0.0.0.0` | Bind IP interface |
| `-api-keys` | `ROUTER_API_KEYS` | `""` | Comma-separated list of static API keys required (disabled if empty) |
| `-db-path` | `ROUTER_DB_PATH` | `keys.db` | Path to the SQLite database file |
| `-admin-key` | `ROUTER_ADMIN_KEY` | `""` | Admin master key for managing dynamic API keys (disabled if empty) |

---

### Using Docker

1. **Build and Run with Docker Compose:**
   ```bash
   docker compose up --build -d
   ```

2. **Check Logs:**
   ```bash
   docker compose logs -f
   ```

The router mounts your local `config.json` as a volume. Any changes to the backends listed in `config.json` will take effect upon container restart.

---

## API Key Authorization & Key Management

The router supports two modes of client API key authorization:
1. **Static Keys**: Defined at startup via the `-api-keys` flag or `ROUTER_API_KEYS` environment variable as a comma-separated string.
2. **Dynamic Keys**: Stored in a lightweight SQLite database (`keys.db`) and managed via admin endpoints on the fly.

When authentication is enabled (by configuring static keys or an admin key), all incoming requests to `/v1/*` must include a valid Bearer token:

```http
Authorization: Bearer <your-client-api-key>
```

If the header is missing or incorrect, the router returns a `401 Unauthorized` status with an OpenAI-compatible error response:

```json
{
  "error": {
    "message": "Incorrect API key provided",
    "type": "invalid_request_error",
    "param": null,
    "code": "invalid_api_key"
  }
}
```

---

## Dynamic Key Management (Admin Endpoints)

If you configure an admin master key (via the `-admin-key` flag or `ROUTER_ADMIN_KEY` environment variable), you can use the following endpoints to manage client API keys dynamically. All admin requests must include the admin master key:

```http
Authorization: Bearer <your-admin-master-key>
```

### 1. Create a New API Key
Generates a new dynamic API key. You can specify a custom `key`, a human-readable `name` (tag/label), and a classification `tag`. If the `key` is not specified, a cryptographically secure token starting with `flm_` is automatically generated.

- **Endpoint**: `POST /admin/keys`
- **Request Body**:
  ```json
  {
    "key": "optional-custom-key",
    "name": "developer-john",
    "tag": "production"
  }
  ```
- **Example Command**:
  ```bash
  curl -X POST http://localhost:8080/admin/keys \
    -H "Authorization: Bearer <admin-master-key>" \
    -H "Content-Type: application/json" \
    -d '{"name": "production-client", "tag": "prod"}'
  ```
- **Response**:
  ```json
  {
    "key": "flm_7f9b8c0d1e2f3a4b5c6d7e8f9a0b1c2d",
    "name": "production-client",
    "tag": "prod",
    "created_at": "2026-06-19T23:25:00Z"
  }
  ```

### 2. List All API Keys
Retrieves the list of all dynamically registered API keys.

- **Endpoint**: `GET /admin/keys`
- **Example Command**:
  ```bash
  curl http://localhost:8080/admin/keys \
    -H "Authorization: Bearer <admin-master-key>"
  ```
- **Response**:
  ```json
  [
    {
      "key": "flm_7f9b8c0d1e2f3a4b5c6d7e8f9a0b1c2d",
      "name": "production-client",
      "tag": "prod",
      "created_at": "2026-06-19T23:25:00Z"
    }
  ]
  ```

### 3. Revoke/Delete an API Key
Deletes the specified API key from the database, instantly revoking its access.

- **Endpoint**: `DELETE /admin/keys/:key`
- **Example Command**:
  ```bash
  curl -X DELETE http://localhost:8080/admin/keys/flm_7f9b8c0d1e2f3a4b5c6d7e8f9a0b1c2d \
    -H "Authorization: Bearer <admin-master-key>"
  ```
- **Response**:
  ```json
  {
    "status": "revoked"
  }
  ```

---

## Testing the API

### 1. List Available Models
```bash
curl http://localhost:8080/v1/models
```
**Response:**
```json
{
  "object": "list",
  "data": [
    {
      "id": "qwen2.5-7b",
      "object": "model",
      "created": 1781900000,
      "owned_by": "fastflowlm"
    }
  ]
}
```

### 2. Standard Chat Completion
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2.5-7b",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": false
  }'
```

### 3. Streaming (SSE) Chat Completion
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2.5-7b",
    "messages": [{"role": "user", "content": "Explain quantum computing in one sentence."}],
    "stream": true
  }'
```

---

## Running Unit Tests

Run the test suite using standard Go testing tools:
```bash
go test -v ./...
```
