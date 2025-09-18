package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

type CEPRequest struct {
	CEP string `json:"cep"`
}

type ErrorResponse struct {
	Message string `json:"message"`
}

var tracer trace.Tracer

func main() {
	// Initialize OpenTelemetry
	ctx := context.Background()
	shutdown, err := initTracer(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize tracer: %v", err)
	}
	defer shutdown()

	tracer = otel.Tracer("service-a")

	// Setup HTTP server with OpenTelemetry instrumentation
	mux := http.NewServeMux()
	mux.HandleFunc("/cep", handleCEP)
	mux.HandleFunc("/health", handleHealth)

	// Wrap the handler with OpenTelemetry instrumentation
	handler := otelhttp.NewHandler(mux, "service-a")

	log.Println("Service A starting on port 8080...")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}

func initTracer(ctx context.Context) (func(), error) {
	// Get OTLP endpoint from environment variable
	otlpEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otlpEndpoint == "" {
		otlpEndpoint = "localhost:4317"
	}

	// Create OTLP trace exporter
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(otlpEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// Create resource
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("service-a"),
			semconv.ServiceVersion("1.0.0"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create trace provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	// Set global trace provider
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			log.Printf("Error shutting down tracer provider: %v", err)
		}
	}, nil
}

func handleCEP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := trace.SpanFromContext(ctx)
	span.SetName("handle-cep-request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req CEPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		writeErrorResponse(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate CEP
	if !isValidCEP(req.CEP) {
		writeErrorResponse(w, "invalid zipcode", http.StatusUnprocessableEntity)
		return
	}

	// Forward to Service B
	if err := forwardToServiceB(ctx, req.CEP, w); err != nil {
		span.RecordError(err)
		log.Printf("Error forwarding to Service B: %v", err)
		writeErrorResponse(w, "internal server error", http.StatusInternalServerError)
		return
	}
}

func isValidCEP(cep string) bool {
	// Check if CEP is exactly 8 digits
	matched, _ := regexp.MatchString(`^\d{8}$`, cep)
	return matched
}

func forwardToServiceB(ctx context.Context, cep string, w http.ResponseWriter) error {
	ctx, span := tracer.Start(ctx, "forward-to-service-b")
	defer span.End()

	serviceBURL := os.Getenv("SERVICE_B_URL")
	if serviceBURL == "" {
		serviceBURL = "http://localhost:8081"
	}

	// Create request payload
	payload := CEPRequest{CEP: cep}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP client with OpenTelemetry instrumentation
	client := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   30 * time.Second,
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, "POST", serviceBURL+"/weather", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make request to Service B: %w", err)
	}
	defer resp.Body.Close()

	// Copy response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	
	if _, err = w.Write(body); err != nil {
		return fmt.Errorf("failed to write response body: %w", err)
	}
	return nil
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
		log.Printf("Failed to write health response: %v", err)
	}
}

func writeErrorResponse(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	
	response := ErrorResponse{Message: message}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
}