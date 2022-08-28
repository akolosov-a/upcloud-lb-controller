package upcloudlb

import (
	"errors"
	"fmt"

	"github.com/UpCloudLtd/upcloud-go-api/v4/upcloud"
	upcloudrequest "github.com/UpCloudLtd/upcloud-go-api/v4/upcloud/request"
	upcloudservice "github.com/UpCloudLtd/upcloud-go-api/v4/upcloud/service"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

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

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(upcloudOperations, upcloudOperationErrors)
}

func New(upcloudSvc *upcloudservice.Service, cfg *UpcloudLbConfig) *UpcloudLb {
	u := UpcloudLb{
		cfg:        cfg,
		upcloudSvc: upcloudSvc,
	}

	return &u
}

func (u *UpcloudLb) Fetch(name string) (bool, error) {
	lbs, err := u.listLoadBalancers()
	if err != nil {
		return false, err
	}

	for _, lb := range *lbs {
		if lb.Name == name {
			u.lb = &lb
			return true, nil
		}
	}

	return false, nil
}

func (u *UpcloudLb) Create(name string) error {
	// If no corresponding LB was found, try to create one
	lb, err := u.createLoadBalancer(name)
	u.lb = lb
	return err
}

func (u *UpcloudLb) listLoadBalancers() (*[]upcloud.LoadBalancer, error) {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancer", "operation": "LIST"}).Inc()
	lbs, err := u.upcloudSvc.GetLoadBalancers(&upcloudrequest.GetLoadBalancersRequest{})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancer", "operation": "LIST"}).Inc()
	}
	return &lbs, err
}

func (u *UpcloudLb) createLoadBalancer(name string) (*upcloud.LoadBalancer, error) {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancer", "operation": "CREATE"}).Inc()
	lb, err := u.upcloudSvc.CreateLoadBalancer(&upcloudrequest.CreateLoadBalancerRequest{
		Name:             name,
		Plan:             u.cfg.Plan,
		Zone:             u.cfg.Zone,
		NetworkUUID:      u.cfg.Network,
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

func (u *UpcloudLb) Reconcile(ports *[]corev1.ServicePort) error {
	// Remove frontends not found in the service ports
	for _, frontend := range u.lb.Frontends {
		if findPort(ports, frontend.Name) != nil {
			continue
		}
		if err := u.deleteFrontend(&frontend); err != nil {
			return err
		}
	}

	// Remove backends not found in the service ports
	for _, backend := range u.lb.Backends {
		if findPort(ports, backend.Name) != nil {
			continue
		}
		if err := u.deleteBackend(&backend); err != nil {
			return err
		}
	}

	return nil
}

func (u *UpcloudLb) ReconcileBackends(ports *[]corev1.ServicePort, nodes *[]corev1.Node) error {
	for _, p := range *ports {
		if err := u.updateBackendFor(&p, nodes); err != nil {
			return err
		}
	}

	return nil
}

func (u *UpcloudLb) ReconcileFrontends(ports *[]corev1.ServicePort) error {
	for _, p := range *ports {
		if err := u.updateFrontendFor(&p); err != nil {
			return err
		}
	}

	return nil
}

func (u *UpcloudLb) DNSName() string {
	return u.lb.DNSName
}

func (u *UpcloudLb) OperationalState() upcloud.LoadBalancerOperationalState {
	return u.lb.OperationalState
}

func (u *UpcloudLb) UUID() string {
	return u.lb.UUID
}

func (u *UpcloudLb) updateBackendFor(port *corev1.ServicePort, nodes *[]corev1.Node) error {
	var b *upcloud.LoadBalancerBackend
	var err error
	name := getPortPersistentName(port)

	b = u.findBackend(name)
	if b == nil {
		b, err = u.createBackend(name)
		if err != nil {
			return err
		}
	}

	membersMap := make(map[string]*upcloud.LoadBalancerBackendMember)
	for _, m := range b.Members {
		membersMap[m.Name] = &m
	}

	nodesMap := make(map[string]*corev1.Node)
	for _, n := range *nodes {
		nodesMap[n.Name] = &n
	}

	targetPort := int(port.NodePort)
	for name, node := range nodesMap {
		member := membersMap[name]
		addr, err := getNodeInternalIP(node)
		if err != nil {
			return err
		}

		if member != nil {
			err = u.updateBackendMember(member, b.Name, addr, targetPort)
		} else {
			_, err = u.createBackendMember(node.Name, b.Name, addr, targetPort)
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

		if err := u.deleteBackendMember(member, b.Name); err != nil {
			return err
		}
	}

	return nil
}

func (u *UpcloudLb) findBackend(name string) *upcloud.LoadBalancerBackend {
	for _, b := range u.lb.Backends {
		if b.Name == name {
			return &b
		}
	}

	return nil
}

func findPort(ports *[]corev1.ServicePort, name string) *corev1.ServicePort {
	for _, port := range *ports {
		if getPortPersistentName(&port) == name {
			return &port
		}
	}
	return nil
}

func getPortPersistentName(p *corev1.ServicePort) string {
	return fmt.Sprintf("port-%d", p.Port)
}

func (u *UpcloudLb) Delete() error {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancer", "operation": "DELETE"}).Inc()
	err := u.upcloudSvc.DeleteLoadBalancer(&upcloudrequest.DeleteLoadBalancerRequest{
		UUID: u.lb.UUID,
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancer", "operation": "DELETE"}).Inc()
	} else {
		u.lb = nil
	}

	return err
}

func (u *UpcloudLb) deleteFrontend(f *upcloud.LoadBalancerFrontend) error {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerFrontend", "operation": "DELETE"}).Inc()
	err := u.upcloudSvc.DeleteLoadBalancerFrontend(&upcloudrequest.DeleteLoadBalancerFrontendRequest{
		ServiceUUID: u.lb.UUID,
		Name:        f.Name,
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerFrontend", "operation": "DELETE"}).Inc()
	}

	return err
}

func (u *UpcloudLb) deleteBackend(b *upcloud.LoadBalancerBackend) error {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackend", "operation": "DELETE"}).Inc()
	err := u.upcloudSvc.DeleteLoadBalancerBackend(&upcloudrequest.DeleteLoadBalancerBackendRequest{
		ServiceUUID: u.lb.UUID,
		Name:        b.Name,
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerBackend", "operation": "DELETE"}).Inc()
	}

	return err
}

func (u *UpcloudLb) createBackend(name string) (*upcloud.LoadBalancerBackend, error) {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackend", "operation": "CREATE"}).Inc()
	b, err := u.upcloudSvc.CreateLoadBalancerBackend(&upcloudrequest.CreateLoadBalancerBackendRequest{
		ServiceUUID: u.lb.UUID,
		Backend: upcloudrequest.LoadBalancerBackend{
			Name:       name,
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

func (u *UpcloudLb) updateFrontendFor(p *corev1.ServicePort) error {
	name := getPortPersistentName(p)
	for _, f := range u.lb.Frontends {
		if f.Name == name {
			return nil
		}
	}

	_, err := u.createFrontend(name, int(p.Port))
	return err
}

func (u *UpcloudLb) createFrontend(name string, portNum int) (*upcloud.LoadBalancerFrontend, error) {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerFrontend", "operation": "CREATE"}).Inc()
	f, err := u.upcloudSvc.CreateLoadBalancerFrontend(&upcloudrequest.CreateLoadBalancerFrontendRequest{
		ServiceUUID: u.lb.UUID,
		Frontend: upcloudrequest.LoadBalancerFrontend{
			Name:           name,
			Mode:           "tcp",
			Port:           portNum,
			DefaultBackend: name,
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

func (u *UpcloudLb) createBackendMember(name string, bName string, addr string, port int) (*upcloud.LoadBalancerBackendMember, error) {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "CREATE"}).Inc()
	m, err := u.upcloudSvc.CreateLoadBalancerBackendMember(&upcloudrequest.CreateLoadBalancerBackendMemberRequest{
		ServiceUUID: u.lb.UUID,
		BackendName: bName,
		Member: upcloudrequest.LoadBalancerBackendMember{
			Name:        name,
			Weight:      1,
			MaxSessions: 1000,
			Enabled:     true,
			Type:        "static",
			IP:          addr,
			Port:        port,
		},
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "CREATE"}).Inc()
	}

	return m, err
}

func (u *UpcloudLb) updateBackendMember(m *upcloud.LoadBalancerBackendMember, bName string, addr string, port int) error {
	// If the member doesn't deviate from the k8s state, do nothing
	if m.IP == addr && m.Port == port {
		return nil
	}

	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "MODIFY"}).Inc()
	_, err := u.upcloudSvc.ModifyLoadBalancerBackendMember(&upcloudrequest.ModifyLoadBalancerBackendMemberRequest{
		ServiceUUID: u.lb.UUID,
		BackendName: bName,
		Member: upcloudrequest.ModifyLoadBalancerBackendMember{
			Name:        m.Name,
			Weight:      upcloud.IntPtr(m.Weight),
			MaxSessions: upcloud.IntPtr(m.MaxSessions),
			Enabled:     upcloud.BoolPtr(m.Enabled),
			Type:        "static",
			IP:          upcloud.StringPtr(addr),
			Port:        port,
		},
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "MODIFY"}).Inc()
	}

	return err
}

func (u *UpcloudLb) deleteBackendMember(m *upcloud.LoadBalancerBackendMember, bName string) error {
	upcloudOperations.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "DELETE"}).Inc()
	err := u.upcloudSvc.DeleteLoadBalancerBackendMember(&upcloudrequest.DeleteLoadBalancerBackendMemberRequest{
		ServiceUUID: u.lb.UUID,
		BackendName: bName,
		Name:        m.Name,
	})
	if err != nil {
		upcloudOperationErrors.With(prometheus.Labels{"resource": "LoadBalancerBackendMember", "operation": "DELETE"}).Inc()
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
