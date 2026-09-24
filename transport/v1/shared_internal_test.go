package v1

import (
	"context"
	_ "embed"
	"testing"

	"github.com/refraction-networking/water"
)

//go:embed testdata/plain.wasm
var internalPlain []byte

// Closing a listener before any Accept must close the prewarmed core it still
// holds; otherwise that core keeps the shared runtime open for as long as the
// listener is retained.
func TestListenerCloseReleasesUnusedPrewarmedCore(t *testing.T) {
	config := &water.Config{TransportModuleBin: internalPlain}
	lis, err := config.ListenContext(context.Background(), "tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	l := lis.(*Listener)
	shared := l.shared
	if l.prewarmed == nil {
		t.Fatal("expected a prewarmed core from version sniffing")
	}
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	if l.prewarmed != nil {
		t.Fatal("prewarmed core still held after Close")
	}

	core := shared.NewCore(context.Background(), config)
	defer core.Close()
	if err := core.Instantiate(); err == nil {
		t.Fatal("shared runtime still open after closing a listener that never accepted")
	}
}

// A supplied core that cannot be adopted keeps serving the first connection
// rather than being closed and replaced, which would drop its imports.
func TestNonAdoptableCoreIsKept(t *testing.T) {
	config := &water.Config{TransportModuleBin: internalPlain}
	core, err := water.NewCoreWithContext(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ImportFunction("env", "water_dial", func(int32, int32, int32, int32) int32 { return 0 }); err != nil {
		t.Fatal(err)
	}
	d, err := NewDialerWithContext(context.Background(), config, core)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.(*Dialer).prewarmed; got != core {
		t.Fatalf("prewarmed = %v, want the supplied core", got)
	}
}

func TestNilConfigReturnsError(t *testing.T) {
	ctx := context.Background()
	if _, err := NewDialerWithContext(ctx, nil, nil); err == nil {
		t.Error("NewDialerWithContext: want error for nil config")
	}
	if _, err := NewFixedDialerWithContext(ctx, nil, nil); err == nil {
		t.Error("NewFixedDialerWithContext: want error for nil config")
	}
	if _, err := NewListenerWithContext(ctx, nil, nil); err == nil {
		t.Error("NewListenerWithContext: want error for nil config")
	}
}
