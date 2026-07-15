package huaweicloud

import (
	"fmt"

	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3/model"
)

// ListenerInfo holds essential information about an ELB listener.
type ListenerInfo struct {
	ID            string
	Name          string
	Protocol      string
	ProtocolPort  int32
	DefaultPoolID string
}

// CreateListener creates a listener on the specified ELB and returns its info.
// protocol should be "TCP" for database workloads (MongoDB/PostgreSQL/MySQL).
func CreateListener(client *elb.ElbClient, elbID, name string, port int32, protocol string) (*ListenerInfo, error) {
	adminStateUp := true
	listenerName := name
	protocolPort := port

	option := model.CreateListenerOption{
		LoadbalancerId: elbID,
		Name:           &listenerName,
		Protocol:       protocol,
		ProtocolPort:   &protocolPort,
		AdminStateUp:   &adminStateUp,
	}

	req := model.CreateListenerRequest{
		Body: &model.CreateListenerRequestBody{
			Listener: &option,
		},
	}

	resp, err := client.CreateListener(&req)
	if err != nil {
		return nil, fmt.Errorf("creating listener %q on ELB %q: %w", name, elbID, err)
	}
	if resp.Listener == nil {
		return nil, fmt.Errorf("create listener response has no listener object")
	}

	return listenerToInfo(resp.Listener), nil
}

// DeleteListener deletes a listener by ID.
// Huawei Cloud cascade-deletes the associated pool, members, and health check.
func DeleteListener(client *elb.ElbClient, listenerID string) error {
	req := model.DeleteListenerRequest{
		ListenerId: listenerID,
	}
	if _, err := client.DeleteListener(&req); err != nil {
		return fmt.Errorf("deleting listener %q: %w", listenerID, err)
	}
	return nil
}

// ListListeners lists all listeners on the specified ELB.
func ListListeners(client *elb.ElbClient, elbID string) ([]ListenerInfo, error) {
	elbIDs := []string{elbID}
	limit := int32(2000) // max page size; one call covers typical ELB listener counts
	req := model.ListListenersRequest{
		LoadbalancerId: &elbIDs,
		Limit:          &limit,
	}

	resp, err := client.ListListeners(&req)
	if err != nil {
		return nil, fmt.Errorf("listing listeners on ELB %q: %w", elbID, err)
	}
	if resp.Listeners == nil {
		return nil, nil
	}

	listeners := *resp.Listeners
	result := make([]ListenerInfo, 0, len(listeners))
	for i := range listeners {
		result = append(result, *listenerToInfo(&listeners[i]))
	}
	return result, nil
}

func listenerToInfo(l *model.Listener) *ListenerInfo {
	return &ListenerInfo{
		ID:            l.Id,
		Name:          l.Name,
		Protocol:      l.Protocol,
		ProtocolPort:  l.ProtocolPort,
		DefaultPoolID: l.DefaultPoolId,
	}
}
