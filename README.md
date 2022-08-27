# UpCloud Load Balancer Controller

UpCloud Load Balancer Controller creates and manages [UpClound loadbalancers](https://upcloud.com/products/managed-load-balancer) for
[Loadbalacner Kubernetes Services](https://kubernetes.io/docs/concepts/services-networking/service/#loadbalancer).

## Features
- Creation of an Upcloud LB pointing to the cluster nodes for every LoadBalacner typed Service resource
- Updating the existing Upcloud LB instance corresponding to a Service in response to changes in its configuration (e.g. ports added or ports deleted)
- Reflecting the LB address in Service resource status.
- **(NOT IMPLEMENTED)** Deletion of an Upcloud LB in response to deletion of the corresponding Service resource

## Demo

1. Create a k8s cluster in the UpCloud using [k3s](https://k3s.io/)
```bash
ZONE=fi-hel1

# Create a private network
upctl network create --name ulc_test --zone ${ZONE} --ip-network address=10.0.10.0/24,dhcp=true
NETWORK_UUID=$(upctl network show ulc_test -o json | jq -r .uuid)

# Create a server with Kubernetes cluster (k3s implementation)
upctl server create --hostname ulc-test --zone ${ZONE} --ssh-keys ~/.ssh/id_rsa.pub --network type=public,family=IPv4 --network type=private,network=03a51e37-6847-4ebe-afc3-413a87859fac,ip-address=10.0.10.3  --enable-metadata --wait
SERVER_IP=$(upctl server show ulc-test -o json | jq -r '.nics[] | select(.type == "public") | .ip_address' | awk -F': ' '{print $2}')

# Install k3s
cat demo/install_k3s.sh | ssh root@${SERVER_IP} sh -
ssh root@${SERVER_IP} cat /etc/rancher/k3s/k3s.yaml | sed "s/127.0.0.1/${SERVER_IP}/" > ./kubeconfig
```

2. Deploy Upcloud LB Controller
```bash
export KUBECONFIG=./kubeconfig
kubectl -n kube-system create secret generic upcloud-credentials --from-literal=UPCLOUD_USERNAME="YOUR UPCLOUD API USERNAME" --from-literal=UPCLOUD_PASSWORD="YOUR UPCLOUD API PASSWORD"`
kubectl -n kube-system create cm upcloud-lb-controller --from-literal=UPCLOUD_ZONE=${ZONE} --from-literal=UPCLOUD_NETWORK=${NETWORK_UUID}
kubectl apply -f deploy/
```

3. Deploy a TCP echo server
```bash
# Create a deployment with echo server and publish its port with an LoadBalancer K8s Service
kuebctl apply demo/echo-server.yaml
```

4. Test connectivity
```bash
LB_ADDRESS=$(kubectl get svc echo-server -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
echo "Is there anybody out there?" | nc ${LB_ADDRESS} 1234
Is there anybody out there?
```
