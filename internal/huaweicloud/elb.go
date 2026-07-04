package huaweicloud

import (
	"fmt"
	"strings"

	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3/model"
)

// ELBInfo holds essential information about a Huawei Cloud ELB.
type ELBInfo struct {
	ID                 string
	Name               string
	ProvisioningStatus string // ACTIVE, PENDING_DELETE
	OperatingStatus    string // ONLINE, FROZEN
	VipAddress         string // Private IPv4 address
	PublicIP           string // Public IPv4 address (empty for internal ELB)
}

// CreateELBOption holds parameters for creating an ELB.
type CreateELBOption struct {
	Name                 string
	VpcID                string
	VipSubnetCidrID      string
	AvailabilityZoneList []string
	IsPublic             bool
	BandwidthSize        int32
	BandwidthChargeMode  string // "traffic" or "bandwidth"
	PublicIPNetworkType  string // "5_bgp" etc.
	Tags                 map[string]string
}

// CreateELB creates a new Huawei Cloud ELB and returns its info.
func CreateELB(client *elb.ElbClient, opt CreateELBOption) (*ELBInfo, error) {
	tags := buildTags(opt.Tags)
	guaranteed := true
	adminStateUp := true
	name := opt.Name
	vpcID := opt.VpcID
	subnetID := opt.VipSubnetCidrID

	lbOption := model.CreateLoadBalancerOption{
		Name:                 &name,
		VpcId:                &vpcID,
		VipSubnetCidrId:      &subnetID,
		AvailabilityZoneList: opt.AvailabilityZoneList,
		Guaranteed:           &guaranteed,
		AdminStateUp:         &adminStateUp,
		Tags:                 &tags,
	}

	if opt.IsPublic {
		lbOption.Publicip = buildPublicIP(opt)
	}

	req := model.CreateLoadBalancerRequest{
		Body: &model.CreateLoadBalancerRequestBody{
			Loadbalancer: &lbOption,
		},
	}

	resp, err := client.CreateLoadBalancer(&req)
	if err != nil {
		return nil, fmt.Errorf("creating ELB %q: %w", opt.Name, err)
	}
	if resp.Loadbalancer == nil {
		return nil, fmt.Errorf("create ELB response has no loadbalancer object")
	}

	return loadBalancerToInfo(resp.Loadbalancer), nil
}

// ShowELB gets ELB details by ID.
func ShowELB(client *elb.ElbClient, id string) (*ELBInfo, error) {
	req := model.ShowLoadBalancerRequest{
		LoadbalancerId: id,
	}

	resp, err := client.ShowLoadBalancer(&req)
	if err != nil {
		return nil, fmt.Errorf("showing ELB %q: %w", id, err)
	}
	if resp.Loadbalancer == nil {
		return nil, fmt.Errorf("show ELB response has no loadbalancer object")
	}

	return loadBalancerToInfo(resp.Loadbalancer), nil
}

// FindELBByName lists ELBs filtered by name and returns the first match.
// Returns (nil, nil) if no ELB with the given name exists.
func FindELBByName(client *elb.ElbClient, name string) (*ELBInfo, error) {
	names := []string{name}
	req := model.ListLoadBalancersRequest{
		Name: &names,
	}

	resp, err := client.ListLoadBalancers(&req)
	if err != nil {
		return nil, fmt.Errorf("listing ELBs by name %q: %w", name, err)
	}
	if resp.Loadbalancers == nil || len(*resp.Loadbalancers) == 0 {
		return nil, nil
	}

	lbs := *resp.Loadbalancers
	return loadBalancerToInfo(&lbs[0]), nil
}

// DeleteELB deletes an ELB by ID.
func DeleteELB(client *elb.ElbClient, id string) error {
	req := model.DeleteLoadBalancerRequest{
		LoadbalancerId: id,
	}

	if _, err := client.DeleteLoadBalancer(&req); err != nil {
		return fmt.Errorf("deleting ELB %q: %w", id, err)
	}
	return nil
}

// loadBalancerToInfo converts a model.LoadBalancer to ELBInfo.
func loadBalancerToInfo(lb *model.LoadBalancer) *ELBInfo {
	info := &ELBInfo{
		ID:                 lb.Id,
		Name:               lb.Name,
		ProvisioningStatus: lb.ProvisioningStatus,
		OperatingStatus:    lb.OperatingStatus,
		VipAddress:         lb.VipAddress,
	}
	if len(lb.Eips) > 0 && lb.Eips[0].EipAddress != nil {
		info.PublicIP = *lb.Eips[0].EipAddress
	}
	return info
}

// buildTags converts a string map to a slice of model.Tag.
func buildTags(tags map[string]string) []model.Tag {
	result := make([]model.Tag, 0, len(tags))
	for k, v := range tags {
		key := k
		val := v
		result = append(result, model.Tag{Key: &key, Value: &val})
	}
	return result
}

// buildPublicIP creates the public IP option for a public ELB.
func buildPublicIP(opt CreateELBOption) *model.CreateLoadBalancerPublicIpOption {
	networkType := opt.PublicIPNetworkType
	if networkType == "" {
		networkType = "5_bgp"
	}

	bwSize := opt.BandwidthSize
	if bwSize == 0 {
		bwSize = 10
	}

	bandwidthName := opt.Name + "-bw"
	ipVersion := int32(4)
	chargeMode := resolveChargeMode(opt.BandwidthChargeMode)
	shareType := model.GetCreateLoadBalancerBandwidthOptionShareTypeEnum().PER

	return &model.CreateLoadBalancerPublicIpOption{
		IpVersion:   &ipVersion,
		NetworkType: networkType,
		Bandwidth: &model.CreateLoadBalancerBandwidthOption{
			Name:       &bandwidthName,
			Size:       &bwSize,
			ChargeMode: &chargeMode,
			ShareType:  &shareType,
		},
	}
}

// resolveChargeMode converts a string to the typed enum.
func resolveChargeMode(mode string) model.CreateLoadBalancerBandwidthOptionChargeMode {
	if strings.EqualFold(mode, "bandwidth") {
		return model.GetCreateLoadBalancerBandwidthOptionChargeModeEnum().BANDWIDTH
	}
	return model.GetCreateLoadBalancerBandwidthOptionChargeModeEnum().TRAFFIC
}

// AnnotationELBID is the Kubernetes annotation for CCE ELB integration.
const AnnotationELBID = "kubernetes.io/elb.id"

// ELBNamePrefix is prepended to the LoadBalancerConfig name to form the ELB name.
const ELBNamePrefix = "elb-"
