package huaweicloud

import (
	"errors"
	"fmt"
	"strings"

	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3/model"
	eipv2 "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/eip/v2"
	eipv2model "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/eip/v2/model"
)

// ELBInfo holds essential information about a Huawei Cloud ELB.
type ELBInfo struct {
	ID                 string
	Name               string
	ProvisioningStatus string // ACTIVE, PENDING_DELETE
	OperatingStatus    string // ONLINE, FROZEN
	VipAddress         string // Private IPv4 address
	PublicIP           string // Public IPv4 address (empty for internal ELB)
	EipID              string // EIP ID for bandwidth update
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

// UpdateELBOption holds parameters for updating an existing ELB.
type UpdateELBOption struct {
	Name                string
	BandwidthSize       int32
	BandwidthChargeMode string // "traffic" or "bandwidth"
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

// UpdateELB updates an existing Huawei Cloud ELB.
func UpdateELB(client *elb.ElbClient, elbID string, opt UpdateELBOption, creds *Credentials) error {
	if opt.Name != "" {
		name := opt.Name
		updateOpt := model.UpdateLoadBalancerOption{
			Name: &name,
		}
		req := model.UpdateLoadBalancerRequest{
			LoadbalancerId: elbID,
			Body: &model.UpdateLoadBalancerRequestBody{
				Loadbalancer: &updateOpt,
			},
		}
		_, err := client.UpdateLoadBalancer(&req)
		if err != nil {
			return fmt.Errorf("updating ELB %q name: %w", elbID, err)
		}
	}

	if opt.BandwidthSize > 0 {
		if err := updateELBBandwidth(elbID, opt.BandwidthSize, opt.BandwidthChargeMode, creds, client); err != nil {
			return fmt.Errorf("updating ELB %q bandwidth: %w", elbID, err)
		}
	}

	return nil
}

// UpdateELBBandwidth is a convenience wrapper for bandwidth-only ELB updates.
func UpdateELBBandwidth(client *elb.ElbClient, elbID string, size int32, chargeMode string, creds *Credentials) error {
	return UpdateELB(client, elbID, UpdateELBOption{
		BandwidthSize:       size,
		BandwidthChargeMode: chargeMode,
	}, creds)
}

// IsNotFoundError returns true if the error indicates the resource was not
// found (HTTP 404). This is more robust than string matching as it checks
// for both typed errors with a StatusCode method and common error message
// patterns returned by the Huawei Cloud SDK.
func IsNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	// Check for errors that implement StatusCode() int (common in HTTP SDKs).
	// errors.As unwraps fmt.Errorf %w chains so wrapped SDK errors are found.
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) {
		return sc.StatusCode() == 404
	}
	// Fall back to error message matching for wrapped SDK errors.
	msg := err.Error()
	return strings.Contains(msg, "404") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "ItemNotFound")
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
	if len(lb.Eips) > 0 {
		if lb.Eips[0].EipAddress != nil {
			info.PublicIP = *lb.Eips[0].EipAddress
		}
		if lb.Eips[0].EipId != nil {
			info.EipID = *lb.Eips[0].EipId
		}
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
// updateELBBandwidth updates the bandwidth of an ELB's EIP using the EIP v2 API.
func updateELBBandwidth(elbID string, size int32, chargeMode string, creds *Credentials, elbClient *elb.ElbClient) error {
	eipClient, err := NewEIPClient(creds)
	if err != nil {
		return fmt.Errorf("creating EIP client: %w", err)
	}

	bandwidthID, err := getBandwidthID(eipClient, elbClient, elbID)
	if err != nil {
		return fmt.Errorf("getting bandwidth ID: %w", err)
	}

	chargeModeEnum := eipResolveChargeMode(chargeMode)
	req := eipv2model.UpdateBandwidthRequest{
		BandwidthId: bandwidthID,
		Body: &eipv2model.UpdateBandwidthRequestBody{
			Bandwidth: &eipv2model.UpdateBandwidthOption{
				Size:       &size,
				ChargeMode: &chargeModeEnum,
			},
		},
	}

	if _, err := eipClient.UpdateBandwidth(&req); err != nil {
		return fmt.Errorf("calling EIP UpdateBandwidth API: %w", err)
	}
	return nil
}

// getBandwidthID retrieves the bandwidth ID for an ELB's EIP.
func getBandwidthID(eipClient *eipv2.EipClient, elbClient *elb.ElbClient, elbID string) (string, error) {
	info, err := ShowELB(elbClient, elbID)
	if err != nil {
		return "", fmt.Errorf("showing ELB to get EIP ID: %w", err)
	}
	if info.EipID == "" {
		return "", errors.New("ELB has no EIP; cannot update bandwidth for internal ELB")
	}

	showReq := eipv2model.ShowPublicipRequest{
		PublicipId: info.EipID,
	}
	showResp, err := eipClient.ShowPublicip(&showReq)
	if err != nil {
		return "", fmt.Errorf("showing public IP %q: %w", info.EipID, err)
	}
	if showResp.Publicip == nil {
		return "", errors.New("show public IP response has no publicip object")
	}
	if showResp.Publicip.BandwidthId == nil || *showResp.Publicip.BandwidthId == "" {
		return "", errors.New("public IP has no bandwidth ID")
	}

	return *showResp.Publicip.BandwidthId, nil
}

// eipResolveChargeMode converts a string to the EIP v2 charge mode enum.
func eipResolveChargeMode(mode string) eipv2model.UpdateBandwidthOptionChargeMode {
	if strings.EqualFold(mode, "traffic") {
		return eipv2model.GetUpdateBandwidthOptionChargeModeEnum().TRAFFIC
	}
	return eipv2model.GetUpdateBandwidthOptionChargeModeEnum().BANDWIDTH
}

// AnnotationELBID is the Kubernetes annotation for CCE ELB integration.
const AnnotationELBID = "kubernetes.io/elb.id"

// ELBNamePrefix is prepended to the LoadBalancerConfig name to form the ELB name.
const ELBNamePrefix = "elb-"

// CreateIPGroup creates an IP address group and returns its ID.
func CreateIPGroup(client *elb.ElbClient, name, description string, cidrs []string) (string, error) {
	ipList := make([]model.CreateIpGroupIpOption, 0, len(cidrs))
	for _, cidr := range cidrs {
		ipList = append(ipList, model.CreateIpGroupIpOption{Ip: cidr})
	}
	groupName := name
	groupDesc := description
	option := model.CreateIpGroupOption{
		Name:        &groupName,
		Description: &groupDesc,
		IpList:      ipList,
	}
	req := model.CreateIpGroupRequest{
		Body: &model.CreateIpGroupRequestBody{
			Ipgroup: &option,
		},
	}
	resp, err := client.CreateIpGroup(&req)
	if err != nil {
		return "", fmt.Errorf("creating IP group %q: %w", name, err)
	}
	if resp.Ipgroup == nil {
		return "", fmt.Errorf("create IP group response has no ipgroup object")
	}
	return resp.Ipgroup.Id, nil
}

// UpdateIPGroup updates the IP list of an existing IP group.
func UpdateIPGroup(client *elb.ElbClient, ipGroupID, name string, cidrs []string) error {
	ipList := make([]model.UpdateIpGroupIpOption, 0, len(cidrs))
	for _, cidr := range cidrs {
		ipList = append(ipList, model.UpdateIpGroupIpOption{Ip: cidr})
	}
	groupName := name
	option := model.UpdateIpGroupOption{
		Name:   &groupName,
		IpList: &ipList,
	}
	req := model.UpdateIpGroupRequest{
		IpgroupId: ipGroupID,
		Body: &model.UpdateIpGroupRequestBody{
			Ipgroup: &option,
		},
	}
	if _, err := client.UpdateIpGroup(&req); err != nil {
		return fmt.Errorf("updating IP group %q: %w", ipGroupID, err)
	}
	return nil
}

// DeleteIPGroup deletes an IP address group.
func DeleteIPGroup(client *elb.ElbClient, ipGroupID string) error {
	req := model.DeleteIpGroupRequest{
		IpgroupId: ipGroupID,
	}
	if _, err := client.DeleteIpGroup(&req); err != nil {
		return fmt.Errorf("deleting IP group %q: %w", ipGroupID, err)
	}
	return nil
}

// FindIPGroupByName lists IP groups by name and returns the first match.
// Uses a limit of 200 to handle up to that many IP groups.
// Returns ("", nil) if no IP group with the given name exists.
func FindIPGroupByName(client *elb.ElbClient, name string) (string, error) {
names := []string{name}
limit := int32(200)
req := model.ListIpGroupsRequest{
Name:  &names,
Limit: &limit,
}
	resp, err := client.ListIpGroups(&req)
	if err != nil {
		return "", fmt.Errorf("listing IP groups by name %q: %w", name, err)
	}
	if resp.Ipgroups == nil || len(*resp.Ipgroups) == 0 {
		return "", nil
	}
	return (*resp.Ipgroups)[0].Id, nil
}
