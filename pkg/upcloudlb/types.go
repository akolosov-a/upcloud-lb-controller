package upcloudlb

// UpcloudLbConfig keeps common configuration options for Upcloud
// Loadbalancers created by this controller
type UpcloudLbConfig struct {
	Plan    string
	Zone    string
	Network string
}
