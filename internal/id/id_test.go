package id_test

import (
	"strings"
	"testing"

	"github.com/atmorouter/atmo-mem/internal/id"
)

func TestNew(t *testing.T) {
	a, err := id.New("mem_")
	if err != nil {
		t.Fatal(err)
	}
	b, err := id.New("mem_")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("ids collided: %s", a)
	}
	if !strings.HasPrefix(a, "mem_") || len(a) != len("mem_")+16 {
		t.Fatalf("id %q has an unexpected shape", a)
	}
}
