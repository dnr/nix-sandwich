package main

import (
	"fmt"
	"log"
	"net/http"
)

type fwHandler func(w http.ResponseWriter, r *http.Request) error
type fwAlive func()

type errWithStatus struct {
	error
	status int
}

func fwErr(status int, format string, a ...any) error {
	return &errWithStatus{
		error:  fmt.Errorf(format, a...),
		status: status,
	}
}

func fwErrE(status int, e error) error {
	return &errWithStatus{
		error:  e,
		status: status,
	}
}

func fw(f fwHandler, alive fwAlive) func(w http.ResponseWriter, r *http.Request) {
	if alive == nil {
		alive = func() {}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		alive()
		defer alive()

		parts := make([]any, 0, 7)
		parts = append(parts, r.Method, " ", r.URL.Path, " ...")
		log.Print(parts...)

		err := f(w, r)

		var status int
		if ewc, ok := err.(*errWithStatus); ok {
			status = ewc.status
		}

		parts = append(parts[:3], " -> ")
		if status > 0 {
			parts = append(parts, status, " ")
		}
		if err != nil {
			parts = append(parts, err)
		} else {
			parts = append(parts, "OK")
		}
		log.Print(parts...)

		if status > 0 {
			w.WriteHeader(status)
		}
	}
}
