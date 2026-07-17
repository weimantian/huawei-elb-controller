package huaweicloud

import (
	"fmt"

	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3/model"
)

// MemberTarget represents a desired backend server member.
// For NodePort mode (instance-type backend), Address is the node IP and
// ProtocolPort is the NodePort. SubnetID is the neutron subnet ID where
// the node resides (required for multi-AZ clusters where nodes span subnets).
type MemberTarget struct {
	Address      string
	ProtocolPort int32
	SubnetID     string
}

// MemberInfo holds essential information about an existing pool member.
type MemberInfo struct {
	ID           string
	Address      string
	ProtocolPort int32
}

// AddMember adds a single member to a pool.
// For NodePort mode, subnetCID is the ELB's subnet ID (required for instance-type backends).
func AddMember(client *elb.ElbClient, poolID, address string, protocolPort int32, subnetCID string) (string, error) {
	adminStateUp := true
	port := protocolPort
	subnet := subnetCID

	option := model.CreateMemberOption{
		Address:      address,
		ProtocolPort: &port,
		SubnetCidrId: &subnet,
		AdminStateUp: &adminStateUp,
	}

	req := model.CreateMemberRequest{
		PoolId: poolID,
		Body: &model.CreateMemberRequestBody{
			Member: &option,
		},
	}

	resp, err := client.CreateMember(&req)
	if err != nil {
		return "", fmt.Errorf("creating member %q:%d on pool %q: %w", address, protocolPort, poolID, err)
	}
	if resp.Member == nil {
		return "", fmt.Errorf("create member response has no member object")
	}
	return resp.Member.Id, nil
}

// DeleteMember deletes a single member from a pool.
func DeleteMember(client *elb.ElbClient, poolID, memberID string) error {
	req := model.DeleteMemberRequest{
		PoolId:   poolID,
		MemberId: memberID,
	}
	if _, err := client.DeleteMember(&req); err != nil {
		return fmt.Errorf("deleting member %q from pool %q: %w", memberID, poolID, err)
	}
	return nil
}

// ListMembers lists all members in a pool.
func ListMembers(client *elb.ElbClient, poolID string) ([]MemberInfo, error) {
	limit := int32(2000)
	req := model.ListMembersRequest{
		PoolId: poolID,
		Limit:  &limit,
	}

	resp, err := client.ListMembers(&req)
	if err != nil {
		return nil, fmt.Errorf("listing members on pool %q: %w", poolID, err)
	}
	if resp.Members == nil {
		return nil, nil
	}

	members := *resp.Members
	result := make([]MemberInfo, 0, len(members))
	for i := range members {
		result = append(result, MemberInfo{
			ID:           members[i].Id,
			Address:      members[i].Address,
			ProtocolPort: members[i].ProtocolPort,
		})
	}
	return result, nil
}

// SyncMembers reconciles the pool's member list to match the desired targets.
// It diffs the current members against desired targets and adds/removes as needed.
// Each MemberTarget must have its own SubnetID (supports multi-AZ clusters).
func SyncMembers(client *elb.ElbClient, poolID string, desired []MemberTarget) error {
	current, err := ListMembers(client, poolID)
	if err != nil {
		return fmt.Errorf("listing current members: %w", err)
	}

	// Build lookup maps for diffing.
	desiredMap := make(map[string]MemberTarget, len(desired))
	for _, d := range desired {
		key := memberKey(d.Address, d.ProtocolPort)
		desiredMap[key] = d
	}

	currentMap := make(map[string]MemberInfo, len(current))
	for _, c := range current {
		key := memberKey(c.Address, c.ProtocolPort)
		currentMap[key] = c
	}

	// Remove members that are no longer desired.
	for key, c := range currentMap {
		if _, exists := desiredMap[key]; !exists {
			if err := DeleteMember(client, poolID, c.ID); err != nil {
				return fmt.Errorf("removing stale member %s: %w", key, err)
			}
		}
	}

	// Add members that are desired but not yet present.
	for key, d := range desiredMap {
		if _, exists := currentMap[key]; !exists {
			if _, err := AddMember(client, poolID, d.Address, d.ProtocolPort, d.SubnetID); err != nil {
				return fmt.Errorf("adding new member %s: %w", key, err)
			}
		}
	}

	return nil
}

func memberKey(address string, port int32) string {
	return fmt.Sprintf("%s:%d", address, port)
}
