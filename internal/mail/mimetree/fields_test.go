package mimetree

import (
	"reflect"
	"testing"
)

func TestFields(t *testing.T) {
	raw := []byte("From: a@example.com\r\nnot a header\r\nSubject: one\r\n\ttwo\r\nFROM : <b@example.com>\r\n\r\nFrom: body@example.com\r\n")
	got := Fields(raw[:HeaderEnd(raw)])
	want := []Field{
		{Name: "from", Value: "a@example.com", Line: "From: a@example.com"},
		{Name: "subject", Value: "one two", Line: "Subject: one two"},
		{Name: "from", Value: "<b@example.com>", Line: "FROM : <b@example.com>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
