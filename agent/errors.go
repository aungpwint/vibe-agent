package agent

import (
	"errors"
	"fmt"
)

// Code is the class of a domain error. The CLI maps it to an exit status, and
// nothing else needs to parse error strings.
type Code string

const (
	CodeNotFound    Code = "not_found"
	CodeConflict    Code = "conflict"
	CodeValidation  Code = "validation"
	CodeUnavailable Code = "unavailable"
)

var (
	ErrProfileNotFound = errors.New("there is no agent profile with that id or slug")
	ErrSessionNotFound = errors.New("there is no session with that id")
	ErrTurnNotFound    = errors.New("there is no turn with that id")
	ErrRunNotFound     = errors.New("there is no run with that id")

	ErrClosed = errors.New("the session is closed")
)

type Error struct {
	Code    Code
	Message string
	Wrapped error
}

func (e *Error) Error() string {
	if e.Wrapped != nil {
		return e.Message + ": " + e.Wrapped.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Wrapped }

// CodeOf classifies any error the domain returns. An error from the driver that
// the domain did not classify is treated as the database being unavailable,
// which is the truthful reading for a local tool.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrProfileNotFound),
		errors.Is(err, ErrSessionNotFound),
		errors.Is(err, ErrTurnNotFound),
		errors.Is(err, ErrRunNotFound),
		errors.Is(err, ErrInvalidID):
		return CodeNotFound
	}
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr.Code
	}
	return CodeUnavailable
}

func invalid(format string, args ...any) error {
	return &Error{Code: CodeValidation, Message: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &Error{Code: CodeConflict, Message: fmt.Sprintf(format, args...)}
}

func unavailable(err error) error {
	return &Error{
		Code:    CodeUnavailable,
		Message: "the local database could not be reached or answered unexpectedly",
		Wrapped: err,
	}
}
