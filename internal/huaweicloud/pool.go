package huaweicloud

import (
	"fmt"

	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3/model"
)

// PoolInfo holds essential information about an ELB backend server group (pool).
type PoolInfo struct {
	ID         string
	Name       string
	Protocol   string
	LbAlgorithm string
}

// CreatePool creates a backend server group associated with a listener.
// protocol should match the listener protocol ("TCP" for database workloads).
// lbAlgorithm is typically "ROUND_ROBIN".
// Returns the new pool ID.
func CreatePool(client *elb.ElbClient, listenerID, name, protocol, lbAlgorithm string) (string, error) {
	adminStateUp := true
	poolName := name
	listenerIDRef := listenerID

	option := model.CreatePoolOption{
		ListenerId:   &listenerIDRef,
		Name:         &poolName,
		Protocol:     protocol,
		LbAlgorithm:  lbAlgorithm,
		AdminStateUp: &adminStateUp,
	}

	req := model.CreatePoolRequest{
		Body: &model.CreatePoolRequestBody{
			Pool: &option,
		},
	}

	resp, err := client.CreatePool(&req)
	if err != nil {
		return "", fmt.Errorf("creating pool %q on listener %q: %w", name, listenerID, err)
	}
	if resp.Pool == nil {
		return "", fmt.Errorf("create pool response has no pool object")
	}
	return resp.Pool.Id, nil
}

// DeletePool deletes a pool by ID.
// The associated health check (if any) is automatically deleted by Huawei Cloud.
// Members under the pool are also deleted.
func DeletePool(client *elb.ElbClient, poolID string) error {
	req := model.DeletePoolRequest{
		PoolId: poolID,
	}
	if _, err := client.DeletePool(&req); err != nil {
		return fmt.Errorf("deleting pool %q: %w", poolID, err)
	}
	return nil
}

// ListPools lists all pools on the specified ELB.
func ListPools(client *elb.ElbClient, elbID string) ([]PoolInfo, error) {
	elbIDs := []string{elbID}
	limit := int32(2000)
	req := model.ListPoolsRequest{
		LoadbalancerId: &elbIDs,
		Limit:          &limit,
	}

	resp, err := client.ListPools(&req)
	if err != nil {
		return nil, fmt.Errorf("listing pools on ELB %q: %w", elbID, err)
	}
	if resp.Pools == nil {
		return nil, nil
	}

	pools := *resp.Pools
	result := make([]PoolInfo, 0, len(pools))
	for i := range pools {
		result = append(result, PoolInfo{
			ID:          pools[i].Id,
			Name:        pools[i].Name,
			Protocol:    pools[i].Protocol,
			LbAlgorithm: pools[i].LbAlgorithm,
		})
	}
	return result, nil
}
