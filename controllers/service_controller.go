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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"net.kolosov/upcloud-lb-controller/pkg/upcloudlbcontroller"
)

var (
	upcloudLbUUIDAnnotation = "upcloud-lb.kolosov.net/lb-uuid"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	UpcloudSvc   *upcloudservice.Service
	UpcloudLbCfg *upcloudlbcontroller.UpcloudLbConfig
}

//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update

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
		log.Info(fmt.Sprintf("Skipping non LoadBalancer service"))
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling LoadBalancer for the service")

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		log.Error(err, "unable to list Nodes")
		return ctrl.Result{}, err
	}
	nodesMap := make(map[string]corev1.Node)
	for _, n := range nodes.Items {
		nodesMap[n.Name] = n
	}

	log.Info("Looking up or creating a LoadBalancer for the service")
	lb, err := r.getUpcloudLoadbalancerForService(&service)
	if err != nil {
		log.Error(err, "unable to get Upcloud Loadbalancer for the service")
		return ctrl.Result{}, err
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
		}
		r.reconcileBackendMembers(lb, backend, &nodesMap, &port)

		frontend, err = r.getUpcloudFrontendForPort(lb, &port)
		if err != nil {
			log.Error(err, "failed to get Upcloud Frontend for a service port", "port", port)
		}
		r.reconcileFrontend(lb, frontend, &port)
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
		lbs, err := r.UpcloudSvc.GetLoadBalancers(&upcloudrequest.GetLoadBalancersRequest{})
		if err != nil {
			return nil, err
		}

		for _, lb := range lbs {
			if lb.DNSName == lbDNSName {
				return &lb, nil
			}
		}
	}

	return r.UpcloudSvc.CreateLoadBalancer(&upcloudrequest.CreateLoadBalancerRequest{
		Name:             fmt.Sprintf("%s-%s", service.Name, service.UID),
		Plan:             r.UpcloudLbCfg.Plan,
		Zone:             r.UpcloudLbCfg.Zone,
		NetworkUUID:      r.UpcloudLbCfg.Network,
		ConfiguredStatus: upcloud.LoadBalancerConfiguredStatusStarted,
		Frontends:        []upcloudrequest.LoadBalancerFrontend{},
		Backends:         []upcloudrequest.LoadBalancerBackend{},
		Resolvers:        []upcloudrequest.LoadBalancerResolver{},
	})
}

func (r *ServiceReconciler) getUpcloudFrontendForPort(lb *upcloud.LoadBalancer, port *corev1.ServicePort) (*upcloud.LoadBalancerFrontend, error) {
	for _, f := range lb.Frontends {
		if f.Name == getPortName(port) {
			return &f, nil
		}
	}

	return r.UpcloudSvc.CreateLoadBalancerFrontend(&upcloudrequest.CreateLoadBalancerFrontendRequest{
		ServiceUUID: lb.UUID,
		Frontend: upcloudrequest.LoadBalancerFrontend{
			Name:           getPortName(port),
			Mode:           "tcp",
			Port:           int(port.Port),
			DefaultBackend: getPortName(port),
			Rules:          []upcloudrequest.LoadBalancerFrontendRule{},
			TLSConfigs:     []upcloudrequest.LoadBalancerFrontendTLSConfig{},
			Properties:     nil,
		},
	})
}

func (r *ServiceReconciler) getUpcloudBackendForPort(lb *upcloud.LoadBalancer, port *corev1.ServicePort) (*upcloud.LoadBalancerBackend, error) {
	for _, b := range lb.Backends {
		if b.Name == getPortName(port) {
			return &b, nil
		}
	}

	return r.UpcloudSvc.CreateLoadBalancerBackend(&upcloudrequest.CreateLoadBalancerBackendRequest{
		ServiceUUID: lb.UUID,
		Backend: upcloudrequest.LoadBalancerBackend{
			Name:       getPortName(port),
			Resolver:   "",
			Members:    []upcloudrequest.LoadBalancerBackendMember{},
			Properties: nil,
		},
	})
}

func (r *ServiceReconciler) reconcileFrontend(lb *upcloud.LoadBalancer, f *upcloud.LoadBalancerFrontend, port *corev1.ServicePort) error {
	if f.Port == int(port.Port) {
		return nil
	}

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
	return err
}

func (r *ServiceReconciler) reconcileBackendMembers(lb *upcloud.LoadBalancer, b *upcloud.LoadBalancerBackend, nodesMap *map[string]corev1.Node, port *corev1.ServicePort) error {
	membersMap := make(map[string]upcloud.LoadBalancerBackendMember)
	for _, m := range b.Members {
		membersMap[m.Name] = m
	}

	for name, node := range *nodesMap {
		member, ok := membersMap[name]
		if !ok {
			r.createBackendMember(lb, b, &node, port)
		} else {
			r.reconcileBackendMember(lb, b, &member, &node, port)
		}
	}

	for name, member := range membersMap {
		_, ok := (*nodesMap)[name]
		if !ok {
			r.deleteBackendMember(lb, b, &member)
		}
	}

	return nil
}

func (r *ServiceReconciler) createBackendMember(lb *upcloud.LoadBalancer, b *upcloud.LoadBalancerBackend, n *corev1.Node, p *corev1.ServicePort) (*upcloud.LoadBalancerBackendMember, error) {
	addr, err := getNodeInternalIP(n)
	if err != nil {
		return nil, err
	}
	return r.UpcloudSvc.CreateLoadBalancerBackendMember(&upcloudrequest.CreateLoadBalancerBackendMemberRequest{
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
}

func (r *ServiceReconciler) reconcileBackendMember(lb *upcloud.LoadBalancer, b *upcloud.LoadBalancerBackend, m *upcloud.LoadBalancerBackendMember, n *corev1.Node, p *corev1.ServicePort) error {
	return nil
}

func (r *ServiceReconciler) deleteBackendMember(lb *upcloud.LoadBalancer, b *upcloud.LoadBalancerBackend, m *upcloud.LoadBalancerBackendMember) error {
	return r.UpcloudSvc.DeleteLoadBalancerBackendMember(&upcloudrequest.DeleteLoadBalancerBackendMemberRequest{
		ServiceUUID: lb.UUID,
		BackendName: b.Name,
		Name:        m.Name,
	})
}

func getNodeInternalIP(n *corev1.Node) (string, error) {
	for _, na := range n.Status.Addresses {
		if na.Type == corev1.NodeInternalIP {
			return na.Address, nil
		}
	}

	return "", errors.New("node has no internal IP address")
}

func getPortName(p *corev1.ServicePort) string {
	if p.Name != "" {
		return p.Name
	}

	return fmt.Sprintf("port-%d", p.Port)
}
