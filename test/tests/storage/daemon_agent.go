//+build daemon_agent

package main

import (
	"fmt"
	"github.com/gitpod-io/gitpod/tests/pkg/integration"
)

func main() {
	integration.ServeAgent(new(DaemonAgent))
}

type DaemonAgent struct {
}

func (*DaemonAgent) CreateBucket(name string, result *bool) error {
	fmt.Println(name)
	*result = true
	return nil
}
