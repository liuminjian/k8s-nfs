package controller

import (
	"context"
	"fmt"
	dockerclient "github.com/docker/docker/client"
	"github.com/golang/glog"
	"github.com/liuminjian/k8s-nfs/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typecode "k8s.io/client-go/kubernetes/typed/core/v1"
	listercore "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

const (
	controllerAgentName   = "nfs-controller"
	SuccessSynced         = "Synced"
	MessageResourceSynced = "Add nfs share path success."
	NFSShareLabel         = "NFSShare"
)

type Controller struct {
	kubeclientset kubernetes.Interface
	podLister     listercore.PodLister
	podSynced     cache.InformerSynced
	workQueue     workqueue.RateLimitingInterface
	recorder      record.EventRecorder
	dockerClient  *dockerclient.Client
	// 用于操作nfs服务容器的id
	containerId string
}

func NewController(kubeclientset kubernetes.Interface, dockerClient *dockerclient.Client,
	podInformer corev1.PodInformer, containerId string) *Controller {

	glog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typecode.EventSinkImpl{
		Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset: kubeclientset,
		podLister:     podInformer.Lister(),
		podSynced:     podInformer.Informer().HasSynced,
		workQueue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "podQueue"),
		recorder:      recorder,
		dockerClient:  dockerClient,
		containerId:   containerId,
	}

	glog.Info("Setting up event handlers")

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueuePod,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*v1.Pod)
			newPod := newObj.(*v1.Pod)
			if oldPod.ResourceVersion == newPod.ResourceVersion {
				return
			}
			controller.enqueuePod(newPod)
		},
		DeleteFunc: controller.enqueuePodForDelete,
	})

	return controller
}

func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workQueue.ShutDown()

	glog.Info("Starting control loop")

	glog.Info("Waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(stopCh, c.podSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")
	return nil
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workQueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {

		defer c.workQueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workQueue.Forget(key)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got :%#v", key))
			return nil
		}

		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}

		c.workQueue.Forget(obj)

		glog.Infof("Successfully synced '%s'", key)

		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}
	return true
}

func (c *Controller) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)

	if err != nil {
		runtime.HandleError(fmt.Errorf("invalied resource key :%s", err.Error()))
		return err
	}

	pod, err := c.podLister.Pods(namespace).Get(name)

	if err != nil {
		if errors.IsNotFound(err) {
			glog.Warningf("Pod: %s/%s does not exist in local cache", namespace, name)
			return nil
		}
		runtime.HandleError(fmt.Errorf("failed to list pod: %s/%s", namespace, name))
		return err
	}

	// pod没启动则返回
	if pod.Status.Phase != v1.PodRunning {
		return nil
	}

	//没使用volumes则返回
	if len(pod.Spec.Volumes) == 0 {
		return nil
	}

	// 检查是否有需要共享的volume
	podId := pod.GetUID()
	labels := pod.GetLabels()
	shareVolumeLabels := labels[NFSShareLabel]
	shareVolumes := strings.Split(shareVolumeLabels, ",")
	for _, conItem := range pod.Spec.Containers {

		for _, vol := range conItem.VolumeMounts {

			if strings.Contains(vol.Name, "default-token") {
				continue
			}

			// 获取volume目录
			volumePath := ""
			for _, volSource := range pod.Spec.Volumes {
				if volSource.Name == vol.Name {
					claimName := volSource.VolumeSource.PersistentVolumeClaim.ClaimName
					pvc, err := c.kubeclientset.CoreV1().PersistentVolumeClaims(namespace).
						Get(claimName, metav1.GetOptions{})
					if err != nil {
						glog.Error(err.Error())
						continue
					}

					// /var/lib/kubelet/pods/<Pod的ID>/volumes/kubernetes.io~<Volume类型>/<Volume名字>
					// empty dir 格式 volumePath = path.Join("/var/lib/kubelet/pods", string(podId), "volumes/kubernetes.io~empty-dir", vol.Name)
					// csi 格式 volumePath = path.Join("/var/lib/kubelet/pods", string(podId), "volumes/kubernetes.io~csi", pvc.Spec.VolumeName, "mount")
					volumePath = path.Join("/var/lib/kubelet/pods", string(podId),
						"volumes/kubernetes.io~csi", pvc.Spec.VolumeName, "mount")
				}
			}

			if volumePath == "" {
				continue
			}

			_, err := os.Stat(volumePath)

			if os.IsNotExist(err) {
				glog.Info("no volume found")
				continue
			}

			nfsItem := fmt.Sprintf("%s\t*(rw,sync,fsid=0,no_subtree_check)", vol.MountPath)

			glog.Infof("nfsItem: %s", nfsItem)

			if InArray(vol.Name, shareVolumes) {

				err := c.Mkdir(vol.MountPath)

				if err != nil {
					continue
				}

				// 如果有delete timestamp，则pod被删除
				if pod.ObjectMeta.DeletionTimestamp != nil {
					glog.Infof("remove nfs item: %s", vol.MountPath)

					err = c.DelNFSItem(vol.MountPath)

					if err != nil {
						continue
					}

					if c.HasMount(vol.MountPath) {

						// 刷新nfs
						c.ExportFs()

						glog.Infof("remove mount: %s", vol.MountPath)

						c.Umount(vol.MountPath)
					}

					continue
				}

				if !c.HasMount(vol.MountPath) {
					glog.Infof("start mount from %s to %s", volumePath, vol.MountPath)
					// 将volume目录挂载到本容器，用nfs共享
					err = c.MountBind(volumePath, vol.MountPath)
					if err != nil {
						continue
					}
				}

				if !c.HasNFSItem(vol.MountPath) {
					glog.Infof("start add nfs item: %s", nfsItem)
					c.AddNFSItem(nfsItem)
				}

			} else {

				if c.HasNFSItem(vol.MountPath) {
					glog.Infof("remove nfs item: %s", vol.MountPath)

					err = c.DelNFSItem(vol.MountPath)

					if err != nil {
						continue
					}
				}

				if c.HasMount(vol.MountPath) {

					// 刷新nfs
					c.ExportFs()

					glog.Infof("remove mount: %s", vol.MountPath)

					c.Umount(vol.MountPath)
				}

			}

		}

	}

	// 刷新nfs
	c.ExportFs()

	c.recorder.Event(pod, v1.EventTypeNormal, SuccessSynced, MessageResourceSynced)

	return nil
}

func execCommand(command string, args []string) error {

	cmd := exec.Command(command, args...)

	out, err := cmd.CombinedOutput()
	glog.Info(string(out))
	if err != nil {
		return fmt.Errorf("cmd: %s, args: %v, output: %s, err: %s", command, args,
			out, err.Error())
	}

	return nil
}

func (c *Controller) ExportFs() {
	glog.Info("exportfs -r")
	rs, _ := utils.Exec(context.Background(), c.dockerClient, c.containerId,
		[]string{"exportfs", "-r"})
	glog.Info(rs.Combined())
}

func (c *Controller) Mkdir(MountPath string) error {
	rs, err := utils.Exec(context.Background(), c.dockerClient, c.containerId,
		[]string{"mkdir", "-p", MountPath})

	if err != nil {
		glog.Errorf("create directory [%s] fail: %s", MountPath, err.Error())
		return err
	}

	glog.Infof("Mkdir output:%s", rs.Combined())

	return nil
}

func (c *Controller) HasMount(mountPath string) bool {
	rs, _ := utils.Exec(context.Background(), c.dockerClient, c.containerId,
		[]string{"sh", "-c", fmt.Sprintf("mount|grep -w %s", mountPath)})
	glog.Info(rs.Stdout())
	return len(rs.Stdout()) > 0
}

func (c *Controller) MountBind(source string, target string) error {
	rs, err := utils.Exec(context.Background(), c.dockerClient, c.containerId,
		[]string{"mount", "-o", "bind", source, target})
	if err != nil {
		glog.Error(err.Error())
		return err
	}
	glog.Infof("MountBind output:%s", rs.Combined())
	return nil
}

func (c *Controller) HasNFSItem(nfsItem string) bool {
	cmdStr := fmt.Sprintf("grep -w '%s' /etc/exports ", nfsItem)
	rs, _ := utils.Exec(context.Background(), c.dockerClient, c.containerId,
		[]string{"sh", "-c", cmdStr})
	glog.Info(rs.Stdout())
	return len(rs.Stdout()) > 0
}

func (c *Controller) DelNFSItem(nfsItem string) error {
	delCmd := fmt.Sprintf("sed -i '/%s/d' /etc/exports", strings.Replace(nfsItem, "/", "\\/", -1))

	rs, err := utils.Exec(context.Background(), c.dockerClient, c.containerId,
		[]string{"sh", "-c", delCmd})

	if err != nil {
		glog.Error(err.Error())
		return err
	}

	glog.Infof("DelNFSItem output:%s", rs.Combined())

	return nil
}

func (c *Controller) AddNFSItem(nfsItem string) {
	appendCmd := fmt.Sprintf("echo '%s' >> /etc/exports", nfsItem)

	rs, err := utils.Exec(context.Background(), c.dockerClient, c.containerId,
		[]string{"sh", "-c", appendCmd})

	if err != nil {
		glog.Error(err.Error())
		return
	}

	glog.Infof("AddNFSItem output:%s", rs.Combined())
}

func (c *Controller) Umount(mountPath string) error {

	rs, err := utils.Exec(context.Background(), c.dockerClient, c.containerId,
		[]string{"umount", mountPath})

	if err != nil {
		glog.Error(err.Error())
		return err
	}

	glog.Infof("Umount output:%s", rs.Combined())
	return nil
}

func InArray(target string, array []string) bool {
	for _, item := range array {
		if item == target {
			return true
		}
	}
	return false
}

func (c *Controller) enqueuePod(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}

	pod := obj.(*v1.Pod)
	labels := pod.GetLabels()
	// 有nfs share label才加入work queue
	if _, ok := labels[NFSShareLabel]; ok {
		c.workQueue.AddRateLimited(key)
	}
}

func (c *Controller) enqueuePodForDelete(obj interface{}) {
	var key string
	var err error
	key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	pod := obj.(*v1.Pod)
	labels := pod.GetLabels()
	// 有nfs share label才加入work queue
	if _, ok := labels[NFSShareLabel]; ok {
		c.workQueue.AddRateLimited(key)
	}
}
