package socket_test

import (
	"runtime"
	"testing"
	"time"

	"github.com/refraction-networking/water/internal/socket"
)

func TestConnPair(t *testing.T) {
	c1, c2, err := socket.ConnPair()
	if err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	time.Sleep(100 * time.Microsecond)

	// test c1 -> c2
	err = testIO(c1, c2, 1000, 1024, 0)
	if err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	time.Sleep(100 * time.Microsecond)

	// test c2 -> c1
	err = testIO(c2, c1, 1000, 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
}
