/*
Copyright 2022.

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

package controllers

import (
	"context"
	"errors"
	"fmt"

	"github.com/UpCloudLtd/upcloud-go-api/v4/upcloud"
	upcloudrequest "github.com/UpCloudLtd/upcloud-go-api/v4/upcloud/request"
	upcloudservice "github.com/UpCloudLtd/upcloud-go-api/v4/upcloud/service"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"net.kolosov/upcloud-lb-controller/pkg/upcloudlbcontroller"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	UpcloudSvc   *upcloudservice.Service
	UpcloudLbCfg *upcloudlbcontroller.UpcloudLbConfig
}

var (
	upcloudOperations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ulc_upcloud_operation_total",
			Help: "Number of operations on Upcloud resources",
		},
		[]string{"resource", "operation"},
	)

	upcloudOperationErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ulc_upcloud_operation_errors_total",
			Help: "Number of failed operations on Upcloud resources",
		},
		[]string{"resource", "operation"},
	)
)

//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(upcloudOperations, upcloudOperationErrors)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Service object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var service corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &service); err != nil {
		log.Error(err, "unable to fetch Service")

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if service.Spec.Type != corev1.ServiceTypeLoadBalancer {
		log.Info("Skipping non LoadBalancer service")
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling LoadBalancer for the service")

	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		log.Error(err, "unable to list Nodes")
		return ctrl.Result{Requeue: true}, err
	}
	nodes := nodeList.Items

	log.Info("Looking up or creating a LoadBalancer for the service")
	lb, err := r.getUpcloudLoadbalancerForService(&service)
	if err != nil {
		log.Error(err, "unable to get Upcloud Loadbalancer for the service")
		return ctrl.Result{Requeue: true}, err
	}
	log.Info("LoadBalancer found for the service", "upcloud-lb", lb.Name)

	service.Status.LoadBalancer = corev1.LoadBalancerStatus{
		Ingress: []corev1.LoadBalancerIngress{
			{Hostname: lb.DNSName},
		},
	}
	if err := r.Status().Update(ctx, &service); err != nil {
		log.Error(err, "unable to update Service status")
		return ctrl.Result{}, err
	}

	// Remove frontends not found in the service ports
	for _, frontend := range lb.Frontends {
		if findPort(&service.Spec.Ports, frontend.Name) != nil {
			continue
		}
		log.Info("Deleting unused frontend", "frontend", frontend)
		if err := r.deleteFrontend(lb, &frontend); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove backends not found in the service ports
	for _, backend := range lb.Backends {
		if findPort(&service.Spec.Ports, backend.Name) != nil {
			continue
		}
		log.Info("Deleting unused backend", "backend", backend)
		if err := r.deleteBackend(lb, &backend); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reconcile frontends and backends corresponding to the
	// existing service ports
	for _, port := range service.Spec.Ports {
		var frontend *upcloud.LoadBalancerFrontend
		var backend *upcloud.LoadBalancerBackend
		var err error

		if port.Protocol != "TCP" {
			log.Info("Skipping non TCP service port")
			continue
		}

		backend, err = r.getUpcloudBackendForPort(lb, &port)
		if err != nil {
			log.Error(err, "failed to get Upcloud Backend for a service port", "port", port)
			return ctrl.Result{}, err
		}
		err = r.reconcileBackend(lb, backend, &nodes, &port)
		if err != nil {
			log.Error(err, "failed to reconcile Upcloud Backend for a service port", "port", port)
			return ctrl.Result{}, err
		}

		frontend, err = r.getUpcloudFrontendForPort(lb, &port)
		if err != nil {
			log.Error(err, "failed to get Upcloud Frontend for a service port", "port", port)
			return ctrl.Result{}, err
		}
		err = r.reconcileFrontend(lb, frontend, &port)
		if err != nil {
			log.Error(err, "failed to reconcile Upcloud Frontend for a service port", "port", port)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
}

func (r *ServiceReconciler) getUpcloudLoadbalancerForService(service *corev1.Service) (*upcloud.LoadBalancer, error) {
	var lbDNSName string

	for _, ingress := range service.Status.LoadBalancer.Ingress {
		lbDNSName = ingress.Hostname
	}

	if lbDNSName != "" {
		upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancer", "operation": "LIST"}).Inc()
		lbs, err := r.UpcloudSvc.GetLoadBalancers(&upcloudrequest.GetLoadBalancersRequest{})
		if err != nil {
			upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancer", "operation": "LIST"}).Inc()
			return nil, err
		}

		for _, lb := range lbs {
			if lb.DNSName == lbDNSName {
				return &lb, nil
			}
		}
	}

	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancer", "operation": "CREATE"}).Inc()
	lb, err := r.UpcloudSvc.CreateLoadBalancer(&upcloudrequest.CreateLoadBalancerRequest{
		Name:             fmt.Sprintf("%s-%s", service.Name, service.UID),
		Plan:             r.UpcloudLbCfg.Plan,
		Zone:             r.UpcloudLbCfg.Zone,
		NetworkUUID:      r.UpcloudLbCfg.Network,
		ConfiguredStatus: upcloud.LoadBalancerConfiguredStatusStarted,
		Frontends:        []upcloudrequest.LoadBalancerFrontend{},
		Backends:         []upcloudrequest.LoadBalancerBackend{},
		Resolvers:        []upcloudrequest.LoadBalancerResolver{},
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancer", "operation": "CREATE"}).Inc()
	}

	return lb, err
}

func (r *ServiceReconciler) getUpcloudFrontendForPort(lb *upcloud.LoadBalancer, port *corev1.ServicePort) (*upcloud.LoadBalancerFrontend, error) {
	portName := getPortPersistentName(port)
	for _, f := range lb.Frontends {
		if f.Name == portName {
			return &f, nil
		}
	}

	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerFrontend", "operation": "CREATE"}).Inc()
	f, err := r.UpcloudSvc.CreateLoadBalancerFrontend(&upcloudrequest.CreateLoadBalancerFrontendRequest{
		ServiceUUID: lb.UUID,
		Frontend: upcloudrequest.LoadBalancerFrontend{
			Name:           portName,
			Mode:           "tcp",
			Port:           int(port.Port),
			DefaultBackend: portName,
			Rules:          []upcloudrequest.LoadBalancerFrontendRule{},
			TLSConfigs:     []upcloudrequest.LoadBalancerFrontendTLSConfig{},
			Properties:     nil,
		},
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerFrontend", "operation": "CREATE"}).Inc()
	}

	return f, err
}

func (r *ServiceReconciler) getUpcloudBackendForPort(lb *upcloud.LoadBalancer, port *corev1.ServicePort) (*upcloud.LoadBalancerBackend, error) {
	portName := getPortPersistentName(port)
	for _, b := range lb.Backends {
		if b.Name == portName {
			return &b, nil
		}
	}

	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackend", "operation": "CREATE"}).Inc()
	b, err := r.UpcloudSvc.CreateLoadBalancerBackend(&upcloudrequest.CreateLoadBalancerBackendRequest{
		ServiceUUID: lb.UUID,
		Backend: upcloudrequest.LoadBalancerBackend{
			Name:       portName,
			Resolver:   "",
			Members:    []upcloudrequest.LoadBalancerBackendMember{},
			Properties: nil,
		},
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerBackend", "operation": "CREATE"}).Inc()
	}

	return b, err
}

func (r *ServiceReconciler) reconcileFrontend(lb *upcloud.LoadBalancer, f *upcloud.LoadBalancerFrontend, port *corev1.ServicePort) error {
	if f.Port == int(port.Port) {
		return nil
	}

	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerFrontend", "operation": "MODIFY"}).Inc()
	_, err := r.UpcloudSvc.ModifyLoadBalancerFrontend(&upcloudrequest.ModifyLoadBalancerFrontendRequest{
		ServiceUUID: lb.UUID,
		Name:        f.Name,
		Frontend: upcloudrequest.ModifyLoadBalancerFrontend{
			Name:           f.Name,
			Mode:           f.Mode,
			Port:           int(port.Port),
			DefaultBackend: f.DefaultBackend,
			Properties:     f.Properties,
		},
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerFrontend", "operation": "MODIFY"}).Inc()
	}

	return err
}

func (r *ServiceReconciler) reconcileBackend(lb *upcloud.LoadBalancer, b *upcloud.LoadBalancerBackend, nodes *[]corev1.Node, port *corev1.ServicePort) error {
	membersMap := make(map[string]*upcloud.LoadBalancerBackendMember)
	for _, m := range b.Members {
		membersMap[m.Name] = &m
	}

	nodesMap := make(map[string]*corev1.Node)
	for _, n := range *nodes {
		nodesMap[n.Name] = &n
	}

	for name, node := range nodesMap {
		var err error
		member := membersMap[name]
		if member != nil {
			err = r.reconcileBackendMember(lb, b, member, node, port)
		} else {
			_, err = r.createBackendMember(lb, b, node, port)
		}

		if err != nil {
			return err
		}
	}

	for name, member := range membersMap {
		// Check if k8s node corresponding to a backend member still exists
		if nodesMap[name] != nil {
			continue
		}

		if err := r.deleteBackendMember(lb, b, member); err != nil {
			return err
		}
	}

	return nil
}

func (r *ServiceReconciler) createBackendMember(lb *upcloud.LoadBalancer, b *upcloud.LoadBalancerBackend, n *corev1.Node, p *corev1.ServicePort) (*upcloud.LoadBalancerBackendMember, error) {
	addr, err := getNodeInternalIP(n)
	if err != nil {
		return nil, err
	}

	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "CREATE"}).Inc()
	m, err := r.UpcloudSvc.CreateLoadBalancerBackendMember(&upcloudrequest.CreateLoadBalancerBackendMemberRequest{
		ServiceUUID: lb.UUID,
		BackendName: b.Name,
		Member: upcloudrequest.LoadBalancerBackendMember{
			Name:        n.Name,
			Weight:      1,
			MaxSessions: 1000,
			Enabled:     true,
			Type:        "static",
			IP:          addr,
			Port:        int(p.NodePort),
		},
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "CREATE"}).Inc()
	}

	return m, err
}

func (r *ServiceReconciler) reconcileBackendMember(lb *upcloud.LoadBalancer, b *upcloud.LoadBalancerBackend, m *upcloud.LoadBalancerBackendMember, n *corev1.Node, p *corev1.ServicePort) error {
	addr, err := getNodeInternalIP(n)
	if err != nil {
		return err
	}

	// If the member doesn't deviate from the k8s state, do nothing
	if m.IP == addr && m.Port == int(p.NodePort) {
		return nil
	}

	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "MODIFY"}).Inc()
	_, err = r.UpcloudSvc.ModifyLoadBalancerBackendMember(&upcloudrequest.ModifyLoadBalancerBackendMemberRequest{
		ServiceUUID: lb.UUID,
		BackendName: b.Name,
		Member: upcloudrequest.ModifyLoadBalancerBackendMember{
			Name:        n.Name,
			Weight:      upcloud.IntPtr(1),
			MaxSessions: upcloud.IntPtr(1000),
			Enabled:     upcloud.BoolPtr(true),
			Type:        "static",
			IP:          upcloud.StringPtr(addr),
			Port:        int(p.NodePort),
		},
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "MODIFY"}).Inc()
	}

	return err
}

func (r *ServiceReconciler) deleteBackendMember(lb *upcloud.LoadBalancer, b *upcloud.LoadBalancerBackend, m *upcloud.LoadBalancerBackendMember) error {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "DELETE"}).Inc()
	err := r.UpcloudSvc.DeleteLoadBalancerBackendMember(&upcloudrequest.DeleteLoadBalancerBackendMemberRequest{
		ServiceUUID: lb.UUID,
		BackendName: b.Name,
		Name:        m.Name,
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "DELETE"}).Inc()
	}

	return err
}

func (r *ServiceReconciler) deleteBackend(lb *upcloud.LoadBalancer, b *upcloud.LoadBalancerBackend) error {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackend", "operation": "DELETE"}).Inc()
	err := r.UpcloudSvc.DeleteLoadBalancerBackend(&upcloudrequest.DeleteLoadBalancerBackendRequest{
		ServiceUUID: lb.UUID,
		Name:        b.Name,
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerBackend", "operation": "DELETE"}).Inc()
	}

	return err
}

func (r *ServiceReconciler) deleteFrontend(lb *upcloud.LoadBalancer, f *upcloud.LoadBalancerFrontend) error {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerFrontend", "operation": "DELETE"}).Inc()
	err := r.UpcloudSvc.DeleteLoadBalancerFrontend(&upcloudrequest.DeleteLoadBalancerFrontendRequest{
		ServiceUUID: lb.UUID,
		Name:        f.Name,
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerFrontend", "operation": "DELETE"}).Inc()
	}

	return err
}

func getNodeInternalIP(n *corev1.Node) (string, error) {
	for _, na := range n.Status.Addresses {
		if na.Type == corev1.NodeInternalIP {
			return na.Address, nil
		}
	}

	return "", errors.New("node has no internal IP address")
}

func getPortPersistentName(p *corev1.ServicePort) string {
	return fmt.Sprintf("port-%d", p.Port)
}

func findPort(ports *[]corev1.ServicePort, name string) *corev1.ServicePort {
	for _, port := range *ports {
		if getPortPersistentName(&port) == name {
			return &port
		}
	}
	return nil
}
