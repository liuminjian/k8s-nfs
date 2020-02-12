# k8s-nfs
用于集群外部通过nfs访问容器存储的插件，采用daemonset + hostport的方式，目前只实现对容器csi存储的访问

### 使用方式
工作节点要开启以下服务
- systemctl start rpcbind.socket
- systemctl start rpcbind

### 使用方式
```yaml
// 编译代码和创建docker镜像
make publish 
// 在各个工作节点下载镜像
// 进入deploy目录，用helm安装
helm install k8s-nfs ./k8s-nfs
// 创建带有csi存储的pod容器
// 这个pod容器需带有NFSShare标签指定哪些volume需要nfs共享，多个volume则逗号分隔，如下
kind: Pod
apiVersion: v1
metadata:
  name: my-app
  labels:
      NFSShare: my-volume
spec:
  containers:
    .....
    .....
  volumes:
    - name: my-volume
      persistentVolumeClaim:
        claimName: csi-pod-pvc
// 最后集群外部就可以通过nfs访问
mount -t nfs 节点ip:/共享目录 /挂载目录
```


