package huaweicloud

import (
	"fmt"

	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/basic"
	ecs "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/ecs/v2"
	ecsmodel "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/ecs/v2/model"
	ecsregion "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/ecs/v2/region"
	vpc "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/vpc/v3"
	vpcmodel "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/vpc/v3/model"
	vpcregion "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/vpc/v3/region"
)

// VPCSubnetInfo holds detected VPC and subnet information.
//
// SubnetID is retained for callers that still need a Neutron subnet ID
// (e.g. as VipSubnetCidrId on the ELB). The ECS-based detector returns it
// from node labels written by the CCE Cloud Controller Manager, not from
// the Huawei Cloud VPC API.
type VPCSubnetInfo struct {
	VPCID    string
	SubnetID string // Neutron subnet ID (for VipSubnetCidrId)
}

// DetectVPCFromECS queries the ECS API for the given server ID and returns
// the VPC ID from the server's metadata. This is the most reliable way to
// determine which VPC a CCE node belongs to, avoiding CIDR overlap issues
// that plague the legacy ListVpcs+CIDR-matching approach.
//
// On CCE, the server's metadata["vpc_id"] is populated automatically by
// the platform — every node in a CCE cluster carries the cluster's VPC ID.
func DetectVPCFromECS(creds *Credentials, serverID string) (string, error) {
	if serverID == "" {
		return "", fmt.Errorf("no server ID provided for VPC detection")
	}

	client, err := newECSClient(creds)
	if err != nil {
		return "", err
	}

	req := &ecsmodel.ShowServerRequest{ServerId: serverID}
	resp, err := client.ShowServer(req)
	if err != nil {
		return "", fmt.Errorf("showing server %s: %w", serverID, err)
	}
	if resp.Server == nil {
		return "", fmt.Errorf("server %s not found", serverID)
	}

	vpcID := resp.Server.Metadata["vpc_id"]
	if vpcID == "" {
		return "", fmt.Errorf("server %s has no vpc_id in metadata", serverID)
	}

	return vpcID, nil
}

// newECSClient creates a Huawei Cloud ECS v2 client from credentials.
func newECSClient(creds *Credentials) (*ecs.EcsClient, error) {
	auth := basic.NewCredentialsBuilder().
		WithAk(creds.AK).
		WithSk(creds.SK).
		WithProjectId(creds.ProjectID).
		Build()

	reg, err := ecsregion.SafeValueOf(creds.Region)
	if err != nil {
		return nil, fmt.Errorf("invalid region %q: %w", creds.Region, err)
	}

	hcClient, err := ecs.EcsClientBuilder().
		WithCredential(auth).
		WithRegion(reg).
		SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("building ECS client: %w", err)
	}

	return ecs.NewEcsClient(hcClient), nil
}

// GetNeutronSubnetID converts a VPC Virsubnet ID (as written by CCE CCM to
// the node.kubernetes.io/subnetid label) into the Neutron subnet ID required
// by the ELB API's VipSubnetCidrId field. The two IDs are different in
// Huawei Cloud: Virsubnet is the VPC service's subnet identifier, while
// the Neutron subnet ID is what ELB expects.
func GetNeutronSubnetID(creds *Credentials, virsubnetID string) (string, error) {
	if virsubnetID == "" {
		return "", fmt.Errorf("no virsubnet ID provided")
	}

	client, err := newVPCClient(creds)
	if err != nil {
		return "", err
	}

	// Query the specific Virsubnet by its ID.
	idFilter := []string{virsubnetID}
	resp, err := client.ListVirsubnets(&vpcmodel.ListVirsubnetsRequest{Id: &idFilter})
	if err != nil {
		return "", fmt.Errorf("listing virsubnet %s: %w", virsubnetID, err)
	}
	if resp.Virsubnets == nil || len(*resp.Virsubnets) == 0 {
		return "", fmt.Errorf("virsubnet %s not found", virsubnetID)
	}

	vs := (*resp.Virsubnets)[0]
	if len(vs.SubnetCidrs) == 0 {
		return "", fmt.Errorf("virsubnet %s has no subnet CIDRs", virsubnetID)
	}

	neutronID := vs.SubnetCidrs[0].Id
	if neutronID == "" {
		return "", fmt.Errorf("virsubnet %s has empty Neutron subnet ID", virsubnetID)
	}

	return neutronID, nil
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
