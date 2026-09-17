// Package errs carries an error from wherever it happened out to whatever has
// to answer for it, with two things attached: the status code that answer
// should use, and a sentence a person can act on.
//
// The two are separate because they have different audiences. The status code
// is for the browser and the logs. The sentence is for somebody who was in the
// middle of buying a ticket, so it says what to do next and never names a Go
// package, a table or a field that only makes sense to whoever wrote the code.
package errs

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is a failure that already knows how it should be answered.
type Error struct {
	Status int    // the HTTP status this should become
	Advice string // what to tell the person, in a sentence they can act on
	Err    error  // what actually went wrong, for the log
}

// New builds an Error whose advice is the whole story.
func New(status int, advice string) *Error {
	return &Error{Status: status, Advice: advice}
}

// Wrap attaches a status and a sentence to an error from further down. The
// wrapped error reaches the log; the sentence reaches the person.
func Wrap(status int, advice string, err error) *Error {
	return &Error{Status: status, Advice: advice, Err: err}
}

// Error reports the underlying failure, which is what a log line wants. It is
// deliberately not the advice: the two strings exist because they are
// different, and a logger that prints the reassuring sentence instead of the
// cause is a logger that cannot be debugged with.
func (e *Error) Error() string {
	if e.Err == nil {
		return e.Advice
	}

	return fmt.Sprintf("%s: %s", e.Advice, e.Err)
}

// Unwrap lets errors.Is and errors.AsType see through to the cause.
func (e *Error) Unwrap() error { return e.Err }

// Answer decides how any error should be answered. An error that never passed
// through this package becomes a 500 with a sentence that admits nothing about
// the internals, because an unplanned failure is exactly the case where the
// message is most likely to leak something.
//
// The app layer calls this rather than switching on error types itself; what
// the app layer decides is which *domain* error it is looking at, using
// errors.AsType, and it does that before it gets here.
func Answer(err error) (int, string) {
	if err == nil {
		return http.StatusOK, ""
	}

	if e, ok := errors.AsType[*Error](err); ok {
		return e.Status, e.Advice
	}

	return http.StatusInternalServerError,
		"Something went wrong on our end. Please try again in a minute, and if it keeps happening let us know what you were doing."
}
