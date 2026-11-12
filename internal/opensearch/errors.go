package opensearch

import (
	"errors"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// withoutRequestURL drops the `"METHOD" "<url>": ` prefix the transport adds
// to errors that never got an HTTP response; the URL carries the configured
// address, including any path, which must not reach logs. The cause (and so
// errors.Is) is kept. When the server answered (responded), err is returned
// as is: it carries the HTTP status and no URL.
func withoutRequestURL(responded bool, err error) error {
	if !responded {
		if cause := errors.Unwrap(err); cause != nil {
			return cause
		}
	}
	return err
}

// stripURL applies withoutRequestURL to the error of an API call, using the
// call's response to tell whether the server answered.
func stripURL(in opensearchapi.Inspect, err error) error {
	return withoutRequestURL(in.Response != nil, err)
}

// ErrPublishStateUnknown is wrapped by the error Publish returns when the
// alias swap failed and the alias could not be shown to be unchanged or to
// point at the new index. The new index may be live: the caller must not
// delete it, and an operator has to check where the products alias points.
var ErrPublishStateUnknown = errors.New("publish state unknown")
