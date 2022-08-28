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
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"net.kolosov/upcloud-lb-controller/pkg/upcloudlb"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	UpcloudSvc   *upcloudservice.Service
	UpcloudLbCfg *upcloudlb.UpcloudLbConfig
	log          logr.Logger
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
//
// This implementation reacts on updates to Service kubernetes
// resources and creates or deletes Upcloud LoadBalancers for
// LoadBalancer typed Services
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	r.log = log

	var service corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &service); err != nil {
		r.log.Error(err, "unable to fetch Service")

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if service.Spec.Type != corev1.ServiceTypeLoadBalancer {
		r.log.Info("Skipping non LoadBalancer service")
		return ctrl.Result{}, nil
	}

	finalizerName := "upcloud-lb-controller.kolosov.net/finalizer"

	// examine DeletionTimestamp to determine if object is under deletion
	if service.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&service, finalizerName) {
			controllerutil.AddFinalizer(&service, finalizerName)
			if err := r.Update(ctx, &service); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(&service, finalizerName) {
			if err := r.deleteExternalResources(&service); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(&service, finalizerName)
			if err := r.Update(ctx, &service); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		r.log.Error(err, "unable to list Nodes")
		return ctrl.Result{Requeue: true}, err
	}

	lb, err := r.reconcileExternalResources(&service, &nodeList.Items)
	if err != nil {
		return ctrl.Result{}, err
	}

	if lb.OperationalState() == "running" {
		r.log.Info("LoadBalancer is ready, updating Service status")
		service.Status.LoadBalancer = corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{
				{Hostname: lb.DNSName()},
			},
		}
		if err := r.Status().Update(ctx, &service); err != nil {
			r.log.Error(err, "unable to update Service status")
			return ctrl.Result{}, err
		}
	} else {
		d, err := time.ParseDuration(requeueDelay)
		if err != nil {
			panic(err)
		}
		r.log.Info(fmt.Sprintf("Loadbalancer is not ready yet, requeue after %s", d))
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

// reconcileExternalResources reconciles the state of the Upcloud
// Loadbalancer corresponding to the given Service resource
func (r *ServiceReconciler) reconcileExternalResources(service *corev1.Service, nodes *[]corev1.Node) (*upcloudlb.UpcloudLb, error) {
	log := r.log

	log.Info("Fetching LoadBalancer for the service")
	lb := upcloudlb.New(r.UpcloudSvc, r.UpcloudLbCfg)
	lbName := fmt.Sprintf("%s-%s", service.Name, service.UID)
	found, err := lb.Fetch(lbName)
	if err != nil {
		log.Error(err, "failed to fetch LoadBalancer for the service")
		return nil, err
	}
	if !found {
		if err := lb.Create(lbName); err != nil {
			log.Error(err, "failed to create LoadBalancer for the service")
			return nil, err
		}
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
	if err = lb.Update(&ports, nodes); err != nil {
		log.Error(err, "Failed to reconcile LoadBalancer")
		return lb, err
	}

	return lb, nil
}

// deleteExternalResources deletes Upcloud Loadbalancer corresponding
// to the given Service resource
func (r *ServiceReconciler) deleteExternalResources(service *corev1.Service) error {
	log := r.log

	lb := upcloudlb.New(r.UpcloudSvc, r.UpcloudLbCfg)
	lbName := fmt.Sprintf("%s-%s", service.Name, service.UID)

	log.Info("Fetching LoadBalancer for the service")
	found, err := lb.Fetch(lbName)
	if err != nil {
		log.Error(err, "failed to fetch LoadBalancer for the service")
		return err
	}
	if !found {
		log.Info("LoadBalancer for Service not found")
		return nil
	}

	log.Info("Deleting LoadBalancer for the service")
	if err := lb.Delete(); err != nil {
		log.Error(err, "failed to delete LoadBalancer for the service")
		return err
	}

	return nil
}
