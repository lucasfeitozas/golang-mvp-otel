package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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

type WeatherResponse struct {
	City  string  `json:"city"`
	TempC float64 `json:"temp_C"`
	TempF float64 `json:"temp_F"`
	TempK float64 `json:"temp_K"`
}

type ErrorResponse struct {
	Message string `json:"message"`
}

type ViaCEPResponse struct {
	CEP         string `json:"cep"`
	Logradouro  string `json:"logradouro"`
	Complemento string `json:"complemento"`
	Bairro      string `json:"bairro"`
	Localidade  string `json:"localidade"`
	UF          string `json:"uf"`
	IBGE        string `json:"ibge"`
	GIA         string `json:"gia"`
	DDD         string `json:"ddd"`
	SIAFI       string `json:"siafi"`
	Erro        bool   `json:"erro,omitempty"`
}

type WeatherAPIResponse struct {
	Location struct {
		Name    string `json:"name"`
		Region  string `json:"region"`
		Country string `json:"country"`
	} `json:"location"`
	Current struct {
		TempC float64 `json:"temp_c"`
		TempF float64 `json:"temp_f"`
	} `json:"current"`
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

	tracer = otel.Tracer("service-b")

	// Setup HTTP server with OpenTelemetry instrumentation
	mux := http.NewServeMux()
	mux.HandleFunc("/weather", handleWeather)
	mux.HandleFunc("/health", handleHealth)

	// Wrap the handler with OpenTelemetry instrumentation
	handler := otelhttp.NewHandler(mux, "service-b")

	log.Println("Service B starting on port 8081...")
	if err := http.ListenAndServe(":8081", handler); err != nil {
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
			semconv.ServiceName("service-b"),
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

func handleWeather(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := trace.SpanFromContext(ctx)
	span.SetName("handle-weather-request")

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

	// Validate CEP format
	if !isValidCEP(req.CEP) {
		writeErrorResponse(w, "invalid zipcode", http.StatusUnprocessableEntity)
		return
	}

	// Get location from ViaCEP
	location, err := getLocationFromCEP(ctx, req.CEP)
	if err != nil {
		span.RecordError(err)
		if err.Error() == "CEP not found" || err.Error() == "can not find zipcode" {
			writeErrorResponse(w, "can not find zipcode", http.StatusNotFound)
		} else {
			log.Printf("Error getting location: %v", err)
			writeErrorResponse(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}

	// Get weather from WeatherAPI
	weather, err := getWeatherFromAPI(ctx, location)
	if err != nil {
		span.RecordError(err)
		log.Printf("Error getting weather: %v", err)
		writeErrorResponse(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Return response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(weather); err != nil {
		log.Printf("Failed to encode weather response: %v", err)
	}
}

func isValidCEP(cep string) bool {
	// Check if CEP is exactly 8 digits
	matched, _ := regexp.MatchString(`^\d{8}$`, cep)
	return matched
}

func getLocationFromCEP(ctx context.Context, cep string) (string, error) {
	ctx, span := tracer.Start(ctx, "get-location-from-cep")
	defer span.End()

	span.SetAttributes(attribute.String("cep", cep))

	// Create HTTP client with OpenTelemetry instrumentation
	client := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}

	// Make request to ViaCEP
	url := fmt.Sprintf("https://viacep.com.br/ws/%s/json/", cep)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to make request to ViaCEP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ViaCEP returned status %d", resp.StatusCode)
	}

	var viaCEPResp ViaCEPResponse
	if err := json.NewDecoder(resp.Body).Decode(&viaCEPResp); err != nil {
		return "", fmt.Errorf("can not find zipcode")
	}

	// Check if CEP was found
	if viaCEPResp.Erro {
		return "", fmt.Errorf("can not find zipcode")
	}

	location := viaCEPResp.Localidade
	span.SetAttributes(attribute.String("location", location))

	return location, nil
}

func getWeatherFromAPI(ctx context.Context, location string) (*WeatherResponse, error) {
	ctx, span := tracer.Start(ctx, "get-weather-from-api")
	defer span.End()

	span.SetAttributes(attribute.String("location", location))

	weatherAPIKey := os.Getenv("WEATHER_API_KEY")
	if weatherAPIKey == "" || weatherAPIKey == "your_weather_api_key_here" {
		// Return mock data for testing when API key is not configured
		span.SetAttributes(attribute.Bool("mock_data", true))
		tempC := 22.5
		return &WeatherResponse{
			City:  location,
			TempC: tempC,
			TempF: celsiusToFahrenheit(tempC),
			TempK: celsiusToKelvin(tempC),
		}, nil
	}

	// Create HTTP client with OpenTelemetry instrumentation
	client := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   10 * time.Second,
	}

	// Make request to WeatherAPI
	apiURL := fmt.Sprintf("http://api.weatherapi.com/v1/current.json?key=%s&q=%s&aqi=no", weatherAPIKey, url.QueryEscape(location))
	log.Printf("Making request to WeatherAPI: %s", apiURL)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request to WeatherAPI: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read response body for detailed error logging
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			log.Printf("WeatherAPI returned status %d, failed to read response body: %v", resp.StatusCode, readErr)
		} else {
			log.Printf("WeatherAPI returned status %d, response body: %s", resp.StatusCode, string(body))
		}
		return nil, fmt.Errorf("WeatherAPI returned status %d", resp.StatusCode)
	}

	var weatherResp WeatherAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&weatherResp); err != nil {
		return nil, fmt.Errorf("failed to decode WeatherAPI response: %w", err)
	}

	// Convert temperatures
	tempC := weatherResp.Current.TempC
	tempF := celsiusToFahrenheit(tempC)
	tempK := celsiusToKelvin(tempC)

	span.SetAttributes(
		attribute.Float64("temp_celsius", tempC),
		attribute.Float64("temp_fahrenheit", tempF),
		attribute.Float64("temp_kelvin", tempK),
	)

	return &WeatherResponse{
		City:  weatherResp.Location.Name,
		TempC: tempC,
		TempF: tempF,
		TempK: tempK,
	}, nil
}

func celsiusToFahrenheit(celsius float64) float64 {
	return celsius*1.8 + 32
}

func celsiusToKelvin(celsius float64) float64 {
	return celsius + 273.15
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
