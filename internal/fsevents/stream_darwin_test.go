//go:build darwin && (amd64 || arm64)

package fsevents

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamReportsAFileUnderTheRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := Start(root, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	file := filepath.Join(root, "sub", "text.txt")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case batch := <-stream.Events:
			for _, event := range batch {
				if event.Path == file && event.Flags&FlagItemCreated != 0 {
					return
				}
			}
		case <-timeout:
			t.Fatal("missing create event")
		}
	}
}

func TestStreamCloseDoesNotWaitForAReader(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := Start(root, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 200 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%03d", index)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	closed := make(chan struct{})
	go func() { stream.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked")
	}
	for range stream.Events {
	}
}
