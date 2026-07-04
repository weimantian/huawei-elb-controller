package huaweicloud

import (
	"fmt"
	"log"
	"net"

	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/basic"
	vpc "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/vpc/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/vpc/v3/model"
	vpcregion "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/vpc/v3/region"
)

// VPCSubnetInfo holds detected VPC and subnet information.
type VPCSubnetInfo struct {
	VPCID    string
	SubnetID string // Neutron subnet ID (for VipSubnetCidrId)
}

// DetectVPCSubnet finds the VPC ID and Neutron subnet ID that contain the given
// node IPs by listing all VPCs and subnets, then matching node IPs against
// subnet CIDRs. Returns an error if nodes span multiple VPCs.
func DetectVPCSubnet(creds *Credentials, nodeIPs []string) (*VPCSubnetInfo, error) {
	if len(nodeIPs) == 0 {
		return nil, fmt.Errorf("no node IPs provided for VPC detection")
	}

	vpcClient, err := newVPCClient(creds)
	if err != nil {
		return nil, err
	}

	// List all VPCs.
	vpcResp, err := vpcClient.ListVpcs(&model.ListVpcsRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing VPCs: %w", err)
	}
	if vpcResp.Vpcs == nil || len(*vpcResp.Vpcs) == 0 {
		return nil, fmt.Errorf("no VPCs found in region %s", creds.Region)
	}

	var detectedVPC string
	var detectedSubnet string

	for _, v := range *vpcResp.Vpcs {
		vpcIDs := []string{v.Id}
		subnetResp, err := vpcClient.ListVirsubnets(&model.ListVirsubnetsRequest{
			VpcId: &vpcIDs,
		})
		if err != nil {
			log.Printf("warning: listing subnets for VPC %s: %v", v.Id, err)
			continue
		}
		if subnetResp.Virsubnets == nil {
			continue
		}

		for _, s := range *subnetResp.Virsubnets {
			for _, sc := range s.SubnetCidrs {
				_, cidr, err := net.ParseCIDR(sc.Cidr)
				if err != nil {
					continue
				}

				for _, ip := range nodeIPs {
					if cidr.Contains(net.ParseIP(ip)) {
						if detectedVPC != "" && detectedVPC != v.Id {
							return nil, fmt.Errorf(
								"nodes span multiple VPCs: %s and %s. "+
									"Please specify huawei-elb.io/vpc-id manually",
								detectedVPC, v.Id,
							)
						}
						detectedVPC = v.Id
						detectedSubnet = sc.Id
					}
				}
			}
		}
	}

	if detectedVPC == "" {
		return nil, fmt.Errorf("could not detect VPC for node IPs: %v", nodeIPs)
	}

	return &VPCSubnetInfo{
		VPCID:    detectedVPC,
		SubnetID: detectedSubnet,
	}, nil
}

// newVPCClient creates a Huawei Cloud VPC v3 client from credentials.
func newVPCClient(creds *Credentials) (*vpc.VpcClient, error) {
	auth := basic.NewCredentialsBuilder().
		WithAk(creds.AK).
		WithSk(creds.SK).
		WithProjectId(creds.ProjectID).
		Build()

	reg, err := vpcregion.SafeValueOf(creds.Region)
	if err != nil {
		return nil, fmt.Errorf("invalid region %q: %w", creds.Region, err)
	}

	hcClient, err := vpc.VpcClientBuilder().
		WithCredential(auth).
		WithRegion(reg).
		SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("building VPC client: %w", err)
	}

	return vpc.NewVpcClient(hcClient), nil
}
