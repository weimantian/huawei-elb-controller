// Command list-vpcs lists Huawei Cloud VPCs and their subnets in the
// configured region. Useful for finding the VPC ID and subnet ID needed
// for LoadBalancerConfig annotations.
package main

import (
	"fmt"
	"os"

	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/basic"
	vpc "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/vpc/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/vpc/v3/model"
	vpcregion "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/vpc/v3/region"

	"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

func main() {
	creds, err := huaweicloud.LoadCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	authBuilder := basic.NewCredentialsBuilder().
		WithAk(creds.AK).
		WithSk(creds.SK).
		WithProjectId(creds.ProjectID)

	if creds.SecurityToken != "" {
		authBuilder = authBuilder.WithSecurityToken(creds.SecurityToken)
	}

	auth := authBuilder.Build()

	reg, err := vpcregion.SafeValueOf(creds.Region)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid region %q: %v\n", creds.Region, err)
		os.Exit(1)
	}

	hcClient, err := vpc.VpcClientBuilder().
		WithCredential(auth).
		WithRegion(reg).
		SafeBuild()
	if err != nil {
		fmt.Fprintf(os.Stderr, "building VPC client: %v\n", err)
		os.Exit(1)
	}
	client := vpc.NewVpcClient(hcClient)

	// List VPCs
	vpcResp, err := client.ListVpcs(&model.ListVpcsRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "listing VPCs: %v\n", err)
		os.Exit(1)
	}
	if vpcResp.Vpcs == nil {
		fmt.Println("No VPCs found.")
		return
	}

	vpcs := *vpcResp.Vpcs
	for _, v := range vpcs {
		fmt.Printf("VPC: %s  ID: %s  CIDR: %s\n", v.Name, v.Id, v.Cidr)

		// List subnets (virsubnets) in this VPC
		vpcIDs := []string{v.Id}
		subnetResp, err := client.ListVirsubnets(&model.ListVirsubnetsRequest{
			VpcId: &vpcIDs,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  listing subnets for VPC %s: %v\n", v.Id, err)
			continue
		}
		if subnetResp.Virsubnets == nil {
			fmt.Println("  (no subnets)")
			continue
		}
		for _, s := range *subnetResp.Virsubnets {
			fmt.Printf("  Subnet: %s  ID: %s  Status: %s  Scope: %s  AZ: %s\n",
				s.Name, s.Id, s.Status, s.Scope, s.ZoneId)
			for _, sc := range s.SubnetCidrs {
				fmt.Printf("    NeutronSubnet: ID=%s  CIDR=%s  GW=%s  IPv=%s\n",
					sc.Id, sc.Cidr, sc.GatewayIp, sc.IpVersion)
			}
		}
	}
}
