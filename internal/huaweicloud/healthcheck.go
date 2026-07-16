package huaweicloud

import (
	"fmt"

	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3/model"
)

// HealthCheckConfig holds parameters for a TCP health check.
type HealthCheckConfig struct {
	Delay      int32 // seconds between checks
	Timeout    int32 // seconds before timeout
	MaxRetries int32 // consecutive successes to mark healthy
}

// DefaultHealthCheckConfig returns sensible defaults for TCP database workloads.
// Aligns with EKS NLB defaults: 10s interval, 10s timeout, 3 retries.
func DefaultHealthCheckConfig() HealthCheckConfig {
	return HealthCheckConfig{
		Delay:      10,
		Timeout:    10,
		MaxRetries: 3,
	}
}

// CreateHealthCheck creates a TCP health check for a pool and returns its ID.
// The health check uses the pool's backend port (monitor_port=null means
// "use the member's protocol_port").
func CreateHealthCheck(client *elb.ElbClient, poolID string, cfg HealthCheckConfig) (string, error) {
	adminStateUp := true
	hcName := "hc-" + truncateStr(poolID, 8)

	option := model.CreateHealthMonitorOption{
		Type:         "TCP",
		PoolId:       poolID,
		Delay:        cfg.Delay,
		Timeout:      cfg.Timeout,
		MaxRetries:   cfg.MaxRetries,
		Name:         &hcName,
		AdminStateUp: &adminStateUp,
	}

	req := model.CreateHealthMonitorRequest{
		Body: &model.CreateHealthMonitorRequestBody{
			Healthmonitor: &option,
		},
	}

	resp, err := client.CreateHealthMonitor(&req)
	if err != nil {
		return "", fmt.Errorf("creating health check on pool %q: %w", poolID, err)
	}
	if resp.Healthmonitor == nil {
		return "", fmt.Errorf("create health check response has no healthmonitor object")
	}
	return resp.Healthmonitor.Id, nil
}

// DeleteHealthCheck deletes a health check by ID.
func DeleteHealthCheck(client *elb.ElbClient, healthCheckID string) error {
	req := model.DeleteHealthMonitorRequest{
		HealthmonitorId: healthCheckID,
	}
	if _, err := client.DeleteHealthMonitor(&req); err != nil {
		return fmt.Errorf("deleting health check %q: %w", healthCheckID, err)
	}
	return nil
}

// ListHealthChecks lists all health checks. Huawei Cloud API does not support
// filtering by pool_id directly, so callers must filter client-side.
func ListHealthChecks(client *elb.ElbClient) ([]HealthMonitorInfo, error) {
	limit := int32(2000)
	var result []HealthMonitorInfo
	var marker *string

	for {
		req := model.ListHealthMonitorsRequest{
			Limit:  &limit,
			Marker: marker,
		}

		resp, err := client.ListHealthMonitors(&req)
		if err != nil {
			return nil, fmt.Errorf("listing health checks: %w", err)
		}
		if resp.Healthmonitors == nil {
			break
		}

		hcs := *resp.Healthmonitors
		for i := range hcs {
			poolID := ""
			if len(hcs[i].Pools) > 0 {
				poolID = hcs[i].Pools[0].Id
			}
			result = append(result, HealthMonitorInfo{
				ID:     hcs[i].Id,
				PoolID: poolID,
				Type:   hcs[i].Type,
			})
		}

		// Check for next page.
		if resp.PageInfo == nil || resp.PageInfo.NextMarker == nil {
			break
		}
		marker = resp.PageInfo.NextMarker
	}
	return result, nil
}

// HealthMonitorInfo holds essential information about a health check.
type HealthMonitorInfo struct {
	ID     string
	PoolID string
	Type   string
}
