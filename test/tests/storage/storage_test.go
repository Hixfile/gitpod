package storage

import (
	"testing"

	"github.com/gitpod-io/gitpod/tests/pkg/integration"
)

func TestCreateBucket(t *testing.T) {
	it := integration.NewTest(t)
	defer it.Done()

	rsa, err := it.Instrument(integration.ComponentWorkspaceDaemon, "daemon")
	if err != nil {
		t.Fatal(err)
		return
	}

	var resp bool
	err = rsa.Call("DaemonAgent.CreateBucket", "foobar", &resp)
	if err != nil {
		t.Fatalf("cannot create bucket: %q", err)
	}
}

type DaemonAgent interface {
	CreateBucket(name string) error
}
