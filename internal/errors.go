package internal

import "fmt"

//
// errors.go
// Copyright (C) 2026 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

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
