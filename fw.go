package main

import (
	"log"
	"net/http"
)

type fwHandler func(w http.ResponseWriter, r *http.Request) (status int, msg string, err error)
type fwAlive func()

func fw(f fwHandler, alive fwAlive) func(w http.ResponseWriter, r *http.Request) {
	if alive == nil {
		alive = func() {}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		alive()
		defer alive()
		log.Print(r.Method, " ", r.URL.Path)
		status, msg, err := f(w, r)
		if err == nil {
			if msg != "" {
				log.Print("  -> ", msg)
			}
		} else if msg == "" {
			log.Print("  -> ", err)
		} else {
			log.Print("  -> ", msg, ": ", err)
		}
		if status > 0 {
			w.WriteHeader(status)
		}
	}
}
