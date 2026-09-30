package internal

//
// errors.go
// Copyright (C) 2026 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"fmt"

	"gitlab.com/tozd/go/errors"
)

type withPublicError struct {
	err error
	msg string
}

func (e withPublicError) Error() string {
	return e.msg
}

func (e withPublicError) Unwrap() error {
	return e.err
}

func (e withPublicError) Details() map[string]any {
	return map[string]any{"public": e.msg}
}

func WithPublic(err error, msg string, args ...any) error {
	return withPublicError{
		err: err,
		msg: fmt.Sprintf(msg, args...),
	}
}

func Public(err error) string {
	msg, _ := errors.AllDetails(err)["public"].(string)

	return msg
}

//------------------------------------------------------------------------------

var ErrNotFound = errors.New("not found")
