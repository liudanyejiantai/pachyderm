package client

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"path"
	"os"

	"github.com/facebookgo/pidfile"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	pachdLocalPort = 30650
	samlAcsLocalPort = 30654
	dashUILocalPort = 30080
	dashWebSocketLocalPort = 30081
	pfsLocalPort = 30652
)

// PortForwarder handles proxying local traffic to a kubernetes pod
type PortForwarder struct {
	core corev1.CoreV1Interface
	client rest.Interface
	config *rest.Config
	namespace string
	stdout io.Writer
	stderr io.Writer
	stopChansLock *sync.Mutex
	stopChans []chan struct{}
	shutdown bool
}

// NewPortForwarder creates a new port forwarder
func NewPortForwarder(namespace string, stdout, stderr io.Writer) (*PortForwarder, error) {
	if namespace == "" {
		namespace = "default"
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, err
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	core := client.CoreV1()

	return &PortForwarder {
		core: core,
		client: core.RESTClient(),
		config: config,
		namespace: namespace,
		stdout: stdout,
		stderr: stderr,
		stopChansLock: &sync.Mutex{},
		stopChans: []chan struct{}{},
		shutdown: false,
	}, nil
}

// Run starts the port forwarder. Returns after initialization is begun,
// returning any initialization errors.
func (f *PortForwarder) Run(appName string, localPort, remotePort int) error {
	podNameSelector := map[string]string {
		"suite": "pachyderm",
		"app": appName,
	}

	podList, err := f.core.Pods(f.namespace).List(metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(metav1.SetAsLabelSelector(podNameSelector)),
		TypeMeta: metav1.TypeMeta{
			Kind:       "ListOptions",
			APIVersion: "v1",
		},
	})
	if err != nil {
		return err
	}
	if len(podList.Items) == 0 {
		return fmt.Errorf("No pods found for app %s", appName)
	}

	// Choose a random pod
	podName := podList.Items[rand.Intn(len(podList.Items))].Name

	url := f.client.Post().
		Resource("pods").
		Namespace(f.namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(f.config)
	if err != nil {
		return err
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)
	ports := []string{fmt.Sprintf("%d:%d", localPort, remotePort)}
	readyChan := make(chan struct{}, 1)
	stopChan := make(chan struct{}, 1)

	// Ensure that the port forwarder isn't already shutdown, and append the
	// shutdown channel so this forwarder can be closed
	f.stopChansLock.Lock()
	if f.shutdown {
		f.stopChansLock.Unlock()
		return fmt.Errorf("port forwarder is shutdown")
	}
	f.stopChans = append(f.stopChans, stopChan)
	f.stopChansLock.Unlock()

	fw, err := portforward.New(dialer, ports, stopChan, readyChan, f.stdout, f.stderr)
	if err != nil {
		return err
	}

	errChan := make(chan error, 1)
	go func() { errChan <- fw.ForwardPorts() }()

	select {
	case err = <- errChan:
		return fmt.Errorf("port forwarding failed: %v", err)
	case <- fw.Ready:
		return nil
	}
}

// RunForDaemon creates a port forwarder for the pachd daemon.
func (f *PortForwarder) RunForDaemon(localPort int) error {
	if localPort == 0 {
		localPort = pachdLocalPort
	}
	return f.Run("pachd", localPort, 650)
}

// RunForSAMLACS creates a port forwarder for SAML ACS.
func (f *PortForwarder) RunForSAMLACS(localPort int) error {
	if localPort == 0 {
		localPort = samlAcsLocalPort
	}
	// TODO(ys): using a suite selector because the original code had that.
	// check if it is necessary.
	return f.Run("pachd", localPort, 654)
}

// RunForDashUI creates a port forwarder for the dash UI.
func (f *PortForwarder) RunForDashUI(localPort int) error {
	if localPort == 0 {
		localPort = dashUILocalPort
	}
	return f.Run("dash", localPort, 8080)
}

// RunForDashWebSocket creates a port forwarder for the dash websocket.
func (f *PortForwarder) RunForDashWebSocket(localPort int) error {
	if localPort == 0 {
		localPort = dashWebSocketLocalPort
	}
	return f.Run("dash", localPort, 8081)
}

// RunForPFS creates a port forwarder for PFS over HTTP.
func (f *PortForwarder) RunForPFS(localPort int) error {
	if localPort == 0 {
		localPort = pfsLocalPort
	}
	return f.Run("pachd", localPort, 30652)
}

// Lock uses pidfiles to ensure that only one port forwarder is running across
// one or more `pachctl` instances
func (f *PortForwarder) Lock() error {
	pidfile.SetPidfilePath(path.Join(os.Getenv("HOME"), ".pachyderm/port-forward.pid"))
	return pidfile.Write()
}

// Close shuts down port forwarding.
func (f *PortForwarder) Close() {
	f.stopChansLock.Lock()
	defer f.stopChansLock.Unlock()

	if f.shutdown {
		panic("port forwarder already shutdown")
	}

	f.shutdown = true

	for _, stopChan := range f.stopChans {
		close(stopChan)
	}
}
