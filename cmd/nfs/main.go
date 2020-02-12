package main

import (
	"flag"
	dockerclient "github.com/docker/docker/client"
	"github.com/golang/glog"
	"github.com/liuminjian/k8s-nfs/pkg/controller"
	"github.com/liuminjian/k8s-nfs/pkg/signals"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"strings"
	"time"
)

var (
	masterURL      string
	kubeConfig     string
	dockerEndPoint string
)

const (
	nfsContainerName = "centos-nfs"
)

func main() {
	flag.Parse()

	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeConfig)

	if err != nil {
		glog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)

	if err != nil {
		glog.Fatalf("Error building kubernetes client: %s", err.Error())
	}

	// 获取同一个pod下另一个nfs容器的id
	containerId := getContainerId(kubeClient)

	if containerId == "" {
		glog.Fatal("container id cannot be found.")
	}

	glog.Infof("Get container id: %s", containerId)

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, time.Second*30)

	// docker client
	dockerClient, err := dockerclient.NewClient(dockerEndPoint, "", nil, nil)
	if err != nil {
		glog.Fatalf("Error init docker client: %s", err.Error())
	}

	ctl := controller.NewController(kubeClient, dockerClient, kubeInformerFactory.Core().V1().Pods(), containerId)

	go kubeInformerFactory.Start(stopCh)

	if err = ctl.Run(2, stopCh); err != nil {
		glog.Fatalf("Error running controller: %s", err.Error())
	}
}

func init() {
	flag.StringVar(&masterURL, "master", "",
		"The address of the Kubernetes API server."+
			" Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&kubeConfig, "kubeConfig", "",
		"Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&dockerEndPoint, "dockerEndPoint", "unix:///var/run/docker.sock",
		"dockerEndPoint")
}

func getContainerId(kubeClient *kubernetes.Clientset) string {
	podName := os.Getenv("POD_NAME")

	if podName == "" {
		glog.Fatal("env pod name not set")
	}

	namespace := os.Getenv("POD_NAMESPACE")

	if namespace == "" {
		glog.Fatal("env pod namespace not set")
	}

	if kubeClient == nil {
		return ""
	}

	curPod, err := kubeClient.CoreV1().Pods(namespace).Get(podName, v1.GetOptions{})

	if err != nil {
		glog.Fatalf("get current pod object fail: %s", err.Error())
	}

	if curPod != nil {

		containerStatuses := curPod.Status.ContainerStatuses

		for _, status := range containerStatuses {
			if status.Name == nfsContainerName {
				return strings.TrimLeft(status.ContainerID, "docker://")
			}
		}

	}
	return ""
}
