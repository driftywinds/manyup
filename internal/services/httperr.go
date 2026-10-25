package services

import "fmt"

// statusError builds the error returned for a non-2xx response.
//
// The exact "status NNN" form matters: internal/uploader/retry.go greps the
// error text for `status (\d{3})` to decide whether an attempt is retryable, so
// a plugin that swallows the status code turns a transient 429/503 into a
// permanent failure. Every service must route HTTP failures through this.
func statusError(service string, status int, body []byte) error {
	return fmt.Errorf("%s: status %d: %s", service, status, describeBody(body))
}
