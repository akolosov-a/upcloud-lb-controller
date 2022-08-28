package upcloudlb

import (
	corev1 "k8s.io/api/core/v1"
	"testing"
)

func TestFindPort(t *testing.T) {
	ports := []corev1.ServicePort{
		{
			Name: "test-port1",
			Port: 80,
		},
		{
			Name: "test-port2",
			Port: 443,
		},
	}

	p := findPort(&ports, "port-80")
	if p == nil {
		t.Errorf("Couldn't find 'port-80'")
	} else if *p != ports[0] {
		t.Errorf("Found port is not 'port-80'")
	}

	p = findPort(&ports, "port-8080")
	if p != nil {
		t.Errorf("Found non existing port 'port-8080'")
	}

}

func TestGetNodeInternalIP(t *testing.T) {
	var tests = []struct {
		name   string
		node   corev1.Node
		result string
		error  bool
	}{
		{
			name:   "node_with_internal_addr",
			result: "192.168.0.1",
			error:  false,
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeExternalIP,
							Address: "1.1.1.1",
						},
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.0.1",
						},
					},
				},
			},
		},
		{
			name:   "node_without_internal_addr",
			result: "",
			error:  true,
			node: corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeExternalIP,
							Address: "1.1.1.1",
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, err := getNodeInternalIP(&tt.node)
			if ip != tt.result {
				t.Errorf("got %s, want %s", ip, tt.result)
			}
			if tt.error && err == nil {
				t.Errorf("error was expected, but no error reported")
			}
			if !tt.error && err != nil {
				t.Errorf("error was not expected, but error reported")
			}
		})
	}
}
