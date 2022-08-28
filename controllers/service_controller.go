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
	"fmt"
	"time"

	upcloudservice "github.com/UpCloudLtd/upcloud-go-api/v4/upcloud/service"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"net.kolosov/upcloud-lb-controller/pkg/upcloudlb"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	UpcloudSvc   *upcloudservice.Service
	UpcloudLbCfg *upcloudlb.UpcloudLbConfig
}

var (
	requeueDelay = "30s"
)

//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

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

	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		log.Error(err, "unable to list Nodes")
		return ctrl.Result{Requeue: true}, err
	}

	log.Info("Fetching LoadBalancer for the service")
	lb, err := upcloudlb.FromK8sService(r.UpcloudSvc, r.UpcloudLbCfg, &service)
	if err != nil {
		log.Error(err, "failed to fetch LoadBalancer for the service")
		return ctrl.Result{}, err
	}
	log = log.WithValues("loadbalacner", lb.UUID())

	ports := []corev1.ServicePort{}
	for _, p := range service.Spec.Ports {
		if p.Protocol != "TCP" {
			log.Info("Skipping non TCP port", "port", p)
			continue
		}
		ports = append(ports, p)
	}
	log = log.WithValues("ports", ports)

	log.Info("Reconciling LoadBalancer for the service")
	if err = lb.Reconcile(&ports); err != nil {
		log.Error(err, "Failed to reconcile LoadBalancer")
		return ctrl.Result{}, err
	}

	if err = lb.ReconcileBackends(&ports, &nodeList.Items); err != nil {
		log.Error(err, "Failed to reconcile LoadBalancer backends")
		return ctrl.Result{}, err
	}

	if err = lb.ReconcileFrontends(&ports); err != nil {
		log.Error(err, "Failed to reconcile LoadBalancer frontends")
		return ctrl.Result{}, err
	}

	if lb.OperationalState() == "running" {
		log.Info("LoadBalancer is ready, updating Service status")
		service.Status.LoadBalancer = corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{
				{Hostname: lb.DNSName()},
			},
		}
		if err := r.Status().Update(ctx, &service); err != nil {
			log.Error(err, "unable to update Service status")
			return ctrl.Result{}, err
		}
	} else {
		d, err := time.ParseDuration(requeueDelay)
		if err != nil {
			panic(err)
		}
		log.Info(fmt.Sprintf("Loadbalancer is not ready yet, requeue after %s", d))
		return ctrl.Result{RequeueAfter: d}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
}
