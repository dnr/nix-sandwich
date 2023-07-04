package main

import (
	"log"
	"net/http"
)

type fwHandler func(w http.ResponseWriter, r *http.Request) (status int, msg string, err error)

func fw(f fwHandler) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
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
