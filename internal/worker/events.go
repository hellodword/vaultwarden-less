package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

const (
	SchemaVersion = 1

	HealthPath = "/healthz"
	EventsPath = "/v1/events"
	CheckPath  = "/v1/checks"
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
	SchemaVersion int `json:"schema_version"`
	Error         struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type requestFailure struct {
	status  int
	code    string
	message string
}

func Handler(worker *Worker) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.RawQuery != "" {
			writeError(writer, http.StatusBadRequest, "usage_error", "Query parameters are not supported")
			return
		}
		switch request.URL.Path {
		case HealthPath, StatusPath:
			if request.Method != http.MethodGet {
				methodNotAllowed(writer, http.MethodGet)
				return
			}
			if !emptyRequestBody(request) {
				writeError(writer, http.StatusBadRequest, "usage_error", "GET requests must have an empty body")
				return
			}
			if request.URL.Path == HealthPath {
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			writeJSON(writer, http.StatusOK, worker.Status())
		case EventsPath, CheckPath:
			if request.Method != http.MethodPost {
				methodNotAllowed(writer, http.MethodPost)
				return
			}
			if failure := decodeEventRequest(writer, request); failure != nil {
				writeError(writer, failure.status, failure.code, failure.message)
				return
			}
			worker.Request()
			if request.URL.Path == EventsPath {
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			writeJSON(writer, http.StatusAccepted, acceptedResponse{SchemaVersion: SchemaVersion, Accepted: true})
		default:
			writeError(writer, http.StatusNotFound, "usage_error", "Endpoint not found")
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

func decodeEventRequest(writer http.ResponseWriter, request *http.Request) *requestFailure {
	if request.Header.Get("Content-Type") != "application/json" {
		return &requestFailure{
			status: http.StatusBadRequest, code: "usage_error",
			message: "Content-Type must be exactly application/json",
		}
	}
	if request.ContentLength > config.EventMaxBytes {
		return &requestFailure{status: http.StatusBadRequest, code: "usage_error", message: "Request body is too large"}
	}
	request.Body = http.MaxBytesReader(writer, request.Body, config.EventMaxBytes)
	data, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &requestFailure{status: http.StatusBadRequest, code: "usage_error", message: "Request body is too large"}
		}
		return &requestFailure{status: http.StatusBadRequest, code: "usage_error", message: "Request body is invalid"}
	}
	if !utf8.Valid(data) {
		return &requestFailure{status: http.StatusBadRequest, code: "usage_error", message: "Request body is invalid"}
	}
	body, err := decodeStrictEventRequest(data)
	if err != nil {
		return &requestFailure{status: http.StatusBadRequest, code: "usage_error", message: "Request must contain only schema_version"}
	}
	if body.SchemaVersion != SchemaVersion {
		return &requestFailure{status: http.StatusBadRequest, code: "unsupported_schema", message: "Request schema_version is unsupported"}
	}
	return nil
}

func decodeStrictEventRequest(data []byte) (eventRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') || !decoder.More() {
		return eventRequest{}, errors.New("invalid request object")
	}
	key, err := decoder.Token()
	if err != nil || key != "schema_version" {
		return eventRequest{}, errors.New("invalid request key")
	}
	var result eventRequest
	if err := decoder.Decode(&result.SchemaVersion); err != nil || decoder.More() {
		return eventRequest{}, errors.New("invalid request value")
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return eventRequest{}, errors.New("invalid request object")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return eventRequest{}, errors.New("trailing request value")
	}
	return result, nil
}

func emptyRequestBody(request *http.Request) bool {
	if request.ContentLength > 0 {
		return false
	}
	if request.Body == nil || request.Body == http.NoBody {
		return true
	}
	var one [1]byte
	read, err := request.Body.Read(one[:])
	return read == 0 && errors.Is(err, io.EOF)
}

func methodNotAllowed(writer http.ResponseWriter, allowed string) {
	writer.Header().Set("Allow", allowed)
	writeError(writer, http.StatusMethodNotAllowed, "usage_error", "Method not allowed")
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	response := errorResponse{SchemaVersion: SchemaVersion}
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
