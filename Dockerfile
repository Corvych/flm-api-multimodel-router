# Stage 1: Build the Go application
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum (if exists) and download dependencies
COPY go.mod go.sum* ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the Go application as a statically linked binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o router main.go

# Stage 2: Create the final lightweight runtime image
FROM alpine:latest

# Install ca-certificates and curl for utilities/health checking
RUN apk --no-cache add ca-certificates curl

WORKDIR /app

# Copy the compiled binary from the builder stage
COPY --from=builder /app/router .

# Expose the default server port
EXPOSE 8080

# Command to run the application
CMD ["./router"]
