package httpapi

import "net/http"

// Error codes of the JSON error envelope.
const (
	codeInvalidParameter = "invalid_parameter"
	codePageOutOfRange   = "page_out_of_range"
	codeSearchUnavail    = "search_unavailable"
	codeInternal         = "internal_error"
)

// apiError is a client-visible error. Messages are fixed text and never echo
// request input.
type apiError struct {
	status  int
	code    string
	message string
}

func invalidParameter(message string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: codeInvalidParameter, message: message}
}

func pageOutOfRange(message string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: codePageOutOfRange, message: message}
}

func searchUnavailable() *apiError {
	return &apiError{status: http.StatusServiceUnavailable, code: codeSearchUnavail, message: "search is temporarily unavailable"}
}

func internalError() *apiError {
	return &apiError{status: http.StatusInternalServerError, code: codeInternal, message: "internal error"}
}
