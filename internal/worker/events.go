package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

const (
	HealthPath = "/healthz"
	EventsPath = "/v1/events"
	CheckPath  = "/v1/check"
	StatusPath = "/v1/status"
)

type eventRequest struct {
	SchemaVersion int `json:"schema_version"`
}

type acceptedResponse struct {
	SchemaVersion int  `json:"schema_version"`
	Accepted      bool `json:"accepted"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type requestError struct {
	status  int
	code    string
	message string
}

func (err *requestError) Error() string { return err.code }

func Handler(worker *Worker) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.RawQuery != "" {
			writeError(writer, http.StatusBadRequest, "invalid_request", "Query parameters are not supported.")
			return
		}
		switch request.URL.Path {
		case HealthPath:
			if request.Method != http.MethodGet {
				methodNotAllowed(writer, http.MethodGet)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		case StatusPath:
			if request.Method != http.MethodGet {
				methodNotAllowed(writer, http.MethodGet)
				return
			}
			writeJSON(writer, http.StatusOK, worker.Status())
		case EventsPath, CheckPath:
			if request.Method != http.MethodPost {
				methodNotAllowed(writer, http.MethodPost)
				return
			}
			if err := decodeEventRequest(writer, request); err != nil {
				var invalid *requestError
				if errors.As(err, &invalid) {
					writeError(writer, invalid.status, invalid.code, invalid.message)
					return
				}
				writeError(writer, http.StatusBadRequest, "invalid_request", "Request must be strict schema v1 JSON.")
				return
			}
			worker.Request()
			if request.URL.Path == EventsPath {
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			writeJSON(writer, http.StatusAccepted, acceptedResponse{SchemaVersion: version.SchemaVersion, Accepted: true})
		default:
			writeError(writer, http.StatusNotFound, "not_found", "Endpoint not found.")
		}
	})
}

func NewHTTPServer(worker *Worker) *http.Server {
	return &http.Server{
		Addr:              config.ListenAddress,
		Handler:           Handler(worker),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

func decodeEventRequest(writer http.ResponseWriter, request *http.Request) error {
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || len(parameters) != 0 {
		return &requestError{
			status: http.StatusUnsupportedMediaType, code: "unsupported_media_type",
			message: "Content-Type must be exactly application/json.",
		}
	}
	request.Body = http.MaxBytesReader(writer, request.Body, config.EventMaxBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var body eventRequest
	if err := decoder.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &requestError{status: http.StatusRequestEntityTooLarge, code: "request_too_large", message: "Request body is too large."}
		}
		return errors.New("invalid request body")
	}
	if body.SchemaVersion != version.SchemaVersion {
		return errors.New("invalid request body")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request contains trailing JSON")
	}
	return nil
}

func methodNotAllowed(writer http.ResponseWriter, allowed string) {
	writer.Header().Set("Allow", allowed)
	writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	response := errorResponse{}
	response.Error.Code = code
	response.Error.Message = message
	writeJSON(writer, status, response)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(value); err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(buffer.Bytes())
}
