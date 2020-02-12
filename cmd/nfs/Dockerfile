FROM alpine:3.10
ADD k8s-nfs /bin/k8s-nfs
RUN chmod +x /bin/k8s-nfs
ENTRYPOINT ["/bin/k8s-nfs", "-alsologtostderr"]