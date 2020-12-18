package integration

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/rpc"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"

	// most of our infra runs on GCP, so it's handy to bake GCP auth support right in
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

func NewTest(t *testing.T) *Test {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("cannot determine user home dir", err)
	}
	kubeconfig := filepath.Join(home, ".kube", "config")

	restConfig, ns, err := getKubeconfig(kubeconfig)
	if err != nil {
		t.Fatal("cannot load kubeconfig", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatal("cannot connecto Kubernetes", err)
	}

	return &Test{
		t:          t,
		Clientset:  client,
		RestConfig: restConfig,
		Namespace:  ns,
	}
}

// GetKubeconfig loads kubernetes connection config from a kubeconfig file
func getKubeconfig(kubeconfig string) (res *rest.Config, namespace string, err error) {
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{},
	)
	namespace, _, err = cfg.Namespace()
	if err != nil {
		return nil, "", err
	}

	res, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, namespace, err
	}

	return res, namespace, nil
}

type Test struct {
	t          *testing.T
	Clientset  kubernetes.Interface
	RestConfig *rest.Config
	Namespace  string

	closer []func() error
}

func (t *Test) Done() {
	for i := len(t.closer) - 1; i >= 0; i-- {
		err := t.closer[i]()
		if err != nil {
			t.t.Logf("cleanup failed: %q", err)
		}
	}
}

func (t *Test) Instrument(component ComponentType, agentName string) (agent *rpc.Client, err error) {
	expectedBinaryName := fmt.Sprintf("gitpod-integration-test-%s-agent", agentName)
	agentLoc, _ := exec.LookPath(expectedBinaryName)
	if agentLoc == "" {
		agentLoc, err = t.buildAgent(agentName)
		if err != nil {
			return nil, err
		}
		defer os.Remove(agentLoc)

		t.t.Log("agent compiled at", agentLoc)
	}

	podName, containerName, err := t.selectPod(component)
	if err != nil {
		return nil, err
	}
	tgtFN := filepath.Base(agentLoc)
	err = t.uploadAgent(agentLoc, tgtFN, podName, containerName)
	if err != nil {
		return nil, err
	}

	localAgentPort, err := getFreePort()
	if err != nil {
		return nil, err
	}

	execErrs := make(chan error, 1)
	go func() {
		defer close(execErrs)
		execErr := t.executeAgent(filepath.Join("/tmp", tgtFN), podName, containerName, localAgentPort)
		if err != nil {
			execErrs <- execErr
		}
	}()
	select {
	case err = <-execErrs:
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("agent stopped unexepectedly")
	case <-time.After(1 * time.Second):
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		if err == nil {
			t.closer = append(t.closer, func() error {
				cancel()
				return nil
			})
		} else {
			cancel()
		}
	}()
	fwdReady, fwdErr := forwardPort(ctx, t.RestConfig, t.Namespace, podName, strconv.Itoa(localAgentPort))
	select {
	case <-fwdReady:
	case err = <-execErrs:
		if err != nil {
			return nil, err
		}
	case err := <-fwdErr:
		if err != nil {
			return nil, err
		}
	}

	res, err := rpc.DialHTTP("tcp", fmt.Sprintf("localhost:%d", localAgentPort))
	if err != nil {
		return nil, err
	}

	t.closer = append(t.closer, func() error {
		return res.Call(MethodTestAgentShutdown, new(TestAgentShutdownRequest), new(TestAgentShutdownResponse))
	})

	return res, nil
}

func getFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ForwardPort establishes a TCP port forwarding to a Kubernetes pod
func forwardPort(ctx context.Context, config *rest.Config, namespace, pod, port string) (readychan chan struct{}, errchan chan error) {
	errchan = make(chan error, 1)
	readychan = make(chan struct{}, 1)

	roundTripper, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		errchan <- err
		return
	}

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, pod)
	hostIP := strings.TrimLeft(config.Host, "https://")
	serverURL := url.URL{Scheme: "https", Path: path, Host: hostIP}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, &serverURL)

	stopChan := make(chan struct{}, 1)
	fwdReadyChan := make(chan struct{}, 1)
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	forwarder, err := portforward.New(dialer, []string{port}, stopChan, fwdReadyChan, out, errOut)
	if err != nil {
		panic(err)
	}

	var once sync.Once
	go func() {
		err := forwarder.ForwardPorts()
		if err != nil {
			errchan <- err
		}
		once.Do(func() { close(readychan) })
	}()

	go func() {
		select {
		case <-readychan:
			// we're out of here
		case <-ctx.Done():
			close(stopChan)
		}
	}()

	go func() {
		for range fwdReadyChan {
		}

		if errOut.Len() != 0 {
			errchan <- fmt.Errorf(errOut.String())
			return
		}

		once.Do(func() { close(readychan) })
	}()

	return
}

func (t *Test) executeAgent(tgtPath string, pod, container string, port int) (err error) {
	restClient := t.Clientset.CoreV1().RESTClient()
	req := restClient.Post().
		Resource("pods").
		Name(pod).
		Namespace(t.Namespace).
		SubResource("exec").
		Param("container", container)
	req.VersionedParams(&corev1.PodExecOptions{
		Container: container,
		Command:   []string{tgtPath, "-rpc-port", strconv.Itoa(port)},
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(t.RestConfig, "POST", req.URL())
	if err != nil {
		return err
	}

	return exec.Stream(remotecommand.StreamOptions{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Tty:    true,
	})
}

func (t *Test) uploadAgent(srcFN, tgtFN string, pod, container string) (err error) {
	stat, err := os.Stat(srcFN)
	if err != nil {
		return xerrors.Errorf("cannot upload agent: %w", err)
	}
	srcIn, err := os.Open(srcFN)
	if err != nil {
		return xerrors.Errorf("cannot upload agent: %w", err)
	}
	defer srcIn.Close()

	tarIn, tarOut := io.Pipe()

	restClient := t.Clientset.CoreV1().RESTClient()
	req := restClient.Post().
		Resource("pods").
		Name(pod).
		Namespace(t.Namespace).
		SubResource("exec").
		Param("container", container)
	req.VersionedParams(&corev1.PodExecOptions{
		Container: container,
		Command:   []string{"tar", "-xmf", "-", "-C", "/tmp"},
		Stdin:     true,
		Stdout:    false,
		Stderr:    false,
		TTY:       false,
	}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(t.RestConfig, "POST", req.URL())
	if err != nil {
		return xerrors.Errorf("cannot upload agent: %w", err)
	}

	var eg errgroup.Group
	eg.Go(func() error {
		err := exec.Stream(remotecommand.StreamOptions{
			Stdin: tarIn,
			Tty:   false,
		})
		if err != nil {
			return xerrors.Errorf("cannot upload agent: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		tarw := tar.NewWriter(tarOut)
		err = tarw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     tgtFN,
			Size:     stat.Size(),
			Mode:     0777,
		})
		if err != nil {
			return xerrors.Errorf("cannot upload agent: %w", err)
		}
		_, err = io.Copy(tarw, srcIn)
		if err != nil {
			return xerrors.Errorf("cannot upload agent: %w", err)
		}
		tarw.Close()
		tarOut.Close()

		return nil
	})
	return eg.Wait()
}

func (t *Test) buildAgent(name string) (loc string, err error) {
	defer func() {
		if err != nil {
			err = xerrors.Errorf("cannot build agent: %w", err)
		}
	}()

	src := name + "_agent.go"
	if _, err := os.Stat(src); err != nil {
		return "", err
	}

	f, err := ioutil.TempFile("", "gitpod-integration-test-*")
	if err != nil {
		return "", err
	}
	f.Close()

	cmd := exec.Command("go", "build", "-o", f.Name(), src)
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, string(out))
	}

	return f.Name(), nil
}

func (t *Test) selectPod(component ComponentType) (pod string, container string, err error) {
	pods, err := t.Clientset.CoreV1().Pods(t.Namespace).List(metav1.ListOptions{
		LabelSelector: "component=" + string(component),
	})
	if err != nil {
		err = xerrors.Errorf("cannot list pods: %w", err)
		return
	}
	if len(pods.Items) == 0 {
		err = xerrors.Errorf("no pods for %s", component)
		return
	}
	p := pods.Items[0]
	pod = p.Name
	if len(pods.Items) > 1 {
		t.t.Logf("found multiple pods for %s, choosing %s", component, pod)
	}

	return
}

type ComponentType string

const (
	ComponentWorkspaceDaemon ComponentType = "ws-daemon"
	CompoentnWorkspace       ComponentType = "workspace"
)
