package integration

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"os/signal"
	"syscall"
)

func ServeAgent(rcvr interface{}) {
	port := flag.Int("rpc-port", 0, "the port on wich to run the RPC server on")
	flag.Parse()

	ta := &testAgent{
		Done: make(chan struct{}),
	}

	err := rpc.RegisterName("TestAgent", ta)
	if err != nil {
		log.Fatalf("cannot register test agent service: %q", err)
	}
	err = rpc.Register(rcvr)
	if err != nil {
		log.Fatalf("cannot register agent service: %q", err)
	}
	rpc.HandleHTTP()
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("cannot start RPC server on :%d", *port)
	}

	fmt.Printf("agent service listening on :%d\n", *port)
	go http.Serve(l, nil)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	fmt.Println("stop with SIGINT or CTRL+C")

	select {
	case <-sigChan:
	case <-ta.Done:
	}
	fmt.Println("shutting down")
}

type testAgent struct {
	Done chan struct{}
}

const (
	MethodTestAgentShutdown = "TestAgent.Shutdown"
)

type TestAgentShutdownRequest struct{}
type TestAgentShutdownResponse struct{}

func (t *testAgent) Shutdown(args *TestAgentShutdownRequest, reply *TestAgentShutdownResponse) error {
	close(t.Done)
	*reply = TestAgentShutdownResponse{}
	return nil
}
