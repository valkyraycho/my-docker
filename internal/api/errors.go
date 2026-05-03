package api

// ErrorResponse is the JSON body returned by the daemon on any non-2xx response.
// The lowercase "message" tag matches Docker's error envelope exactly.
type ErrorResponse struct {
	Message string `json:"message"`
}
