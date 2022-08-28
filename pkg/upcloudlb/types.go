package upcloudlb

import (
	"github.com/UpCloudLtd/upcloud-go-api/v4/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v4/upcloud/service"
)

type UpcloudLbConfig struct {
	Plan    string
	Zone    string
	Network string
}

type UpcloudLb struct {
	cfg        *UpcloudLbConfig
	upcloudSvc *service.Service
	lb         *upcloud.LoadBalancer
}
