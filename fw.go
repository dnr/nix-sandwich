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
		log.Print(r.Method, " ", r.URL.Path, " ...")
		err := f(w, r)
		var status int
		if ewc := err.(*errWithStatus); ewc != nil {
			status = ewc.status
		}
		if err != nil {
			log.Print(r.Method, " ", r.URL.Path, " -> ", status, " ", err)
		} else {
			log.Print(r.Method, " ", r.URL.Path, status, " OK")
		}
		if status > 0 {
			w.WriteHeader(status)
		}
	}
}
