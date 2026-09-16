package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeDrainsInFlightRequest(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("finished"))
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, listener, func(c context.Context) { <-c.Done() }, time.Second) }()
	response := make(chan string, 1)
	go func() {
		r, e := http.Get("http://" + listener.Addr().String())
		if e != nil {
			response <- e.Error()
			return
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		response <- string(b)
	}()
	<-entered
	cancel()
	select {
	case e := <-done:
		t.Fatalf("shutdown returned before handler finished: %v", e)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if got := <-response; got != "finished" {
		t.Fatal(got)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
