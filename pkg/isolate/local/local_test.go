package local_test

import (
	"io"
	"os"
	"testing"

	"github.com/urmzd/playscape/pkg/isolate/isolatetest"
	"github.com/urmzd/playscape/pkg/isolate/local"
)

func TestConformance(t *testing.T) {
	isolatetest.Run(t, local.Driver{Log: io.Discard}, isolatetest.Options{
		Image: "host",
		Env:   map[string]string{"PATH": os.Getenv("PATH")},
	})
}
