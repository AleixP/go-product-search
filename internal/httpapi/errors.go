package httpapi

import "net/http"

// Error codes of the JSON error envelope.
const (
	codeInvalidParameter = "invalid_parameter"
	codePageOutOfRange   = "page_out_of_range"
	codeNotFound         = "not_found"
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

func notFound() *apiError {
	return &apiError{status: http.StatusNotFound, code: codeNotFound, message: "product not found"}
}

func noParameters() *apiError {
	return invalidParameter("unknown parameter; this endpoint accepts no parameters")
}
