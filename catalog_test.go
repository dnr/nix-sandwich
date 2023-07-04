package main

import (
	"reflect"
	"testing"
)

func TestFindDashes(t *testing.T) {
	for _, pair := range []struct {
		s string
		e []int
	}{
		{"", nil},
		{"eeeeeeeee", nil},
		{"asdf-eeeeee", []int{4}},
		{"asdf--eeeee", []int{4, 5}},
		{"-tttttt", []int{0}},
		{"---oooooo", []int{0, 1, 2}},
		{"qqqqqq-", []int{6}},
		{"qqqqqq--", []int{6, 7}},
		{"qqqqqq---", []int{6, 7, 8}},
		{"-a-b--cdef-", []int{0, 2, 4, 5, 10}},
	} {
		if !reflect.DeepEqual(pair.e, findDashes(pair.s)) {
			t.Error(pair)
		}
	}
}

func TestMatchLen(t *testing.T) {
	for _, args := range []struct {
		a, b string
		e    int
	}{
		{"", "", 0},
		{"a", "", 0},
		{"", "b", 0},
		{"b", "b", 1},
		{"bcccc", "b", 1},
		{"bcccc", "bcc", 3},
		{"uiopuiop", "uiopuiop", 8},
		{"uiOpuiop", "uiopuiop", 2},
	} {
		if args.e != matchLen(args.a, args.b) {
			t.Error(args)
		}
	}
}
