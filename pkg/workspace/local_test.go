package workspace_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/playscape/pkg/workspace"
)

func TestLocalRoundTrip(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := ws.Write(ctx, "runs/1/out.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	rc, err := ws.Read(ctx, "runs/1/out.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != "hello" {
		t.Fatalf("read back %q, %v", got, err)
	}

	if err := ws.Delete(ctx, "runs/1/out.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Read(ctx, "runs/1/out.txt"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}

func TestLocalList(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"runs/2/a", "runs/1/a", "logs/x"} {
		if err := ws.Write(ctx, k, strings.NewReader("v")); err != nil {
			t.Fatal(err)
		}
	}

	keys, err := ws.List(ctx, "runs")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"runs/1/a", "runs/2/a"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("List(runs) = %v, want %v", keys, want)
	}

	all, err := ws.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List(\"\") = %v, want 3 keys", all)
	}
}

func TestLocalRejectsInvalidKeys(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"", "/abs", "a/../b", "./x", "trailing/"} {
		if err := ws.Write(ctx, k, strings.NewReader("v")); !errors.Is(err, workspace.ErrInvalidKey) {
			t.Errorf("Write(%q): want ErrInvalidKey, got %v", k, err)
		}
	}
}
