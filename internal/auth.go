package internal

//
// auth.go
// Copyright (C) 2026 Karol Będkowski <Karol Będkowski@kkomp>
//
// Distributed under terms of the GPLv3 license.
//

import (
	"context"
	"log/slog"
	"net/http"

	slogctx "github.com/veqryn/slog-context"
	"golang.org/x/crypto/bcrypt"
)

type UserCtxKeyType string

const UserCtxKey = UserCtxKeyType("user_login")

type Authenticator struct {
	next  http.Handler
	users map[string]string
}

func (a *Authenticator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, _ := r.BasicAuth()
	switch userpass, ok := a.users[user]; {
	case user == "":
		// do no log initial, pre-auth errors
	case !ok:
		slog.InfoContext(r.Context(), "server: auth failed", "user", user, "reason", "unknown user")
	case ok && a.authenticate(pass, userpass):
		ctx := slogctx.Append(r.Context(), slog.String("user", user))
		ctx = context.WithValue(ctx, UserCtxKey, user)
		r = r.WithContext(ctx)

		a.next.ServeHTTP(w, r)

		return
	default:
		slog.InfoContext(r.Context(), "server: auth failed", "user", user, "reason", "wrong password")
	}

	w.Header().Set("WWW-Authenticate", `Basic realm="widdlerex"`)
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

func (a *Authenticator) authenticate(providedpass, userpass string) bool {
	return bcrypt.CompareHashAndPassword([]byte(userpass), []byte(providedpass)) == nil
}

func UserFromCtx(ctx context.Context) string {
	user, _ := ctx.Value(UserCtxKey).(string)

	return user
}
