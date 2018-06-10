/*
Copyright 2016-2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth/testauthority"
	kubeutils "github.com/gravitational/teleport/lib/kube/utils"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
	"gopkg.in/check.v1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	//	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

var _ = check.Suite(&KubeSuite{})

type KubeSuite struct {
	*kubernetes.Clientset
	kubeConfig *rest.Config
	ports      utils.PortList
	me         *user.User
	// priv/pub pair to avoid re-generating it
	priv []byte
	pub  []byte

	// kubeCACert is a certificate of a kubernetes certificate authority
	kubeCACert []byte
}

func (s *KubeSuite) SetUpSuite(c *check.C) {
	var err error
	utils.InitLoggerForTests()
	SetTestTimeouts(time.Millisecond * time.Duration(100))

	s.priv, s.pub, err = testauthority.New().GenerateKeyPair("")
	c.Assert(err, check.IsNil)

	s.ports, err = utils.GetFreeTCPPorts(AllocatePortsNum, utils.PortStartingNumber+AllocatePortsNum+1)
	if err != nil {
		c.Fatal(err)
	}
	s.me, _ = user.Current()

	// close & re-open stdin because 'go test' runs with os.stdin connected to /dev/null
	stdin, err := os.Open("/dev/tty")
	if err != nil {
		os.Stdin.Close()
		os.Stdin = stdin
	}

	testEnabled := os.Getenv(teleport.KubeRunTests)
	if ok, _ := strconv.ParseBool(testEnabled); !ok {
		c.Skip("Skipping Kubernetes test suite.")
	}

	kubeConfigPath := os.Getenv("KUBECONFIG")
	s.Clientset, s.kubeConfig, err = kubeutils.GetKubeClient(kubeConfigPath)
	c.Assert(err, check.IsNil)

	ns := newNamespace(testNamespace)
	_, err = s.Core().Namespaces().Create(ns)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			c.Fatalf("Failed to create namespace: %v.", err)
		}
	}

	// fetch certificate authority cert, by grabbing it
	// from the DNS app pod that is always running
	set := labels.Set{"k8s-app": "kube-dns"}
	pods, err := s.Core().Pods("kube-system").List(metav1.ListOptions{
		LabelSelector: set.AsSelector().String(),
	})
	c.Assert(err, check.IsNil)
	if len(pods.Items) == 0 {
		c.Fatalf("Failed to find kube-dns pods.")
	}
	log.Infof("Found %v pods", len(pods.Items))
	pod := pods.Items[0]

	out := &bytes.Buffer{}
	err = kubeExec(s.kubeConfig, kubeExecArgs{
		podName:      pod.Name,
		podNamespace: pod.Namespace,
		container:    "kubedns",
		command:      []string{"/bin/cat", teleport.KubeCAPath},
		stdout:       out,
	})
	c.Assert(err, check.IsNil)
	s.kubeCACert = out.Bytes()
	log.Infof("Got CA Cert: <%v>", string(s.kubeCACert))
}

func (s *KubeSuite) TearDownSuite(c *check.C) {
	var err error
	// restore os.Stdin to its original condition: connected to /dev/null
	os.Stdin.Close()
	os.Stdin, err = os.Open("/dev/null")
	c.Assert(err, check.IsNil)
}

func (s *KubeSuite) SetUpTest(c *check.C) {

}

// TestKubeProxy tests kubernetes proxy feature set - exec, attach, logs
func (s *KubeSuite) TestKubeProxy(c *check.C) {
	log.Infof("Running Test Kube Proxy: %v", s.Clientset)
}

const (
	testTimeout = 1 * time.Minute

	testNamespace = "teletest"
)

func newNamespace(name string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

type kubeExecArgs struct {
	podName      string
	podNamespace string
	container    string
	command      []string
	stdout       io.Writer
	stderr       io.Writer
	stdin        io.Reader
	tty          bool
}

func toBoolString(val bool) string {
	return fmt.Sprintf("%t", val)
}

// kubeExec executes command against kubernetes API server
func kubeExec(kubeConfig *rest.Config, args kubeExecArgs) error {
	if args.stdin == nil {
		args.stdin = &bytes.Buffer{}
	}
	query := make(url.Values)
	for _, arg := range args.command {
		query.Add("command", arg)
	}
	if args.stdout != nil {
		query.Set("stdout", "true")
	}
	if args.stdin != nil {
		// if this option is ever set to true,
		// the call hangs, figure out why?
		//		query.Set("stdin", "true")
	}
	if args.stderr != nil {
		query.Set("stderr", "true")
	}
	if args.tty {
		query.Set("tty", "true")
	}
	query.Set("container", args.container)
	u, err := url.Parse(kubeConfig.Host)
	if err != nil {
		return trace.Wrap(err)
	}
	u.Scheme = "https"
	u.Path = fmt.Sprintf("/api/v1/namespaces/%v/pods/%v/exec", args.podNamespace, args.podName)
	u.RawQuery = query.Encode()
	executor, err := remotecommand.NewSPDYExecutor(kubeConfig, "POST", u)
	if err != nil {
		return trace.Wrap(err)
	}
	opts := remotecommand.StreamOptions{
		Stdin:  args.stdin,
		Stdout: args.stdout,
		Stderr: args.stderr,
		Tty:    args.tty,
	}
	return executor.Stream(opts)
}
