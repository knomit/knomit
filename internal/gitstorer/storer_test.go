package gitstorer_test

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"knomit/internal/gitstorer"
)

func TestObjectRoundTrip(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Write a blob
	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, _ := obj.Writer()
	w.Write([]byte("hello world"))
	w.Close()

	hash, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("SetEncodedObject: %v", err)
	}

	// Read it back
	got, err := s.EncodedObject(plumbing.BlobObject, hash)
	if err != nil {
		t.Fatalf("EncodedObject: %v", err)
	}
	r, _ := got.Reader()
	buf := make([]byte, 11)
	r.Read(buf)
	if string(buf) != "hello world" {
		t.Fatalf("got %q, want %q", buf, "hello world")
	}
}

func TestIterEncodedObjects(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, content := range []string{"foo", "bar"} {
		obj := s.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		w, err := obj.Writer()
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
		w.Close()
		if _, err := s.SetEncodedObject(obj); err != nil {
			t.Fatal(err)
		}
	}

	iter, err := s.IterEncodedObjects(plumbing.BlobObject)
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()

	count := 0
	if err := iter.ForEach(func(obj plumbing.EncodedObject) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 objects, got %d", count)
	}
}

func TestEncodedObjectAnyType(t *testing.T) {
	s, err := gitstorer.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, _ := obj.Writer()
	w.Write([]byte("any-type-test"))
	w.Close()
	hash, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}

	// Should find with AnyObject
	got, err := s.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		t.Fatalf("EncodedObject(AnyObject): %v", err)
	}
	if got.Type() != plumbing.BlobObject {
		t.Fatalf("expected BlobObject, got %v", got.Type())
	}
}
