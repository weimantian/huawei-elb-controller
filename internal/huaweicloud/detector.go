package huaweicloud

import (
"context"
"fmt"
"sort"
"sync"
"time"

corev1 "k8s.io/api/core/v1"
"sigs.k8s.io/controller-runtime/pkg/client"
)

// DetectedParams holds the cluster's VPC/subnet/AZ detected from nodes.
type DetectedParams struct {
	VPCID    string
	SubnetID string
	AZs      []string
}
const detectorCacheTTL = 1 * time.Hour

// NetworkDetector detects network parameters (VPC, subnet, AZs) from
// Kubernetes cluster nodes. Result is cached for 1 hour after the first
// successful detection since all CCE nodes share the same VPC/subnet.
type NetworkDetector struct {
Credentials *Credentials

detectMu   sync.Mutex
detected   *DetectedParams
detectedAt time.Time
}

// NewNetworkDetector creates a NetworkDetector with the given credentials.
func NewNetworkDetector(creds *Credentials) *NetworkDetector {
	return &NetworkDetector{Credentials: creds}
}

// Detect detects VPC ID, subnet ID, and availability zones from the cluster's
// nodes. The result is cached on first success — subsequent calls return the
// cached values without querying the API again.
func (d *NetworkDetector) Detect(ctx context.Context, c client.Client) (vpcID, subnetID string, azs []string, err error) {
	d.detectMu.Lock()
	defer d.detectMu.Unlock()

	if d.detected != nil && time.Since(d.detectedAt) < detectorCacheTTL {
return d.detected.VPCID, d.detected.SubnetID, d.detected.AZs, nil
}

	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList, &client.ListOptions{Limit: 1}); err != nil {
		return "", "", nil, fmt.Errorf("listing nodes: %w", err)
	}
	if len(nodeList.Items) == 0 {
		return "", "", nil, fmt.Errorf("no nodes found in cluster")
	}

	node := nodeList.Items[0]

	virsubnetID := node.Labels["node.kubernetes.io/subnetid"]
	if virsubnetID == "" {
		return "", "", nil, fmt.Errorf(
			"node %s has no node.kubernetes.io/subnetid label", node.Name)
	}

	subnetID, err = GetNeutronSubnetID(d.Credentials, virsubnetID)
	if err != nil {
		return "", "", nil, fmt.Errorf("converting virsubnet to neutron subnet: %w", err)
	}

	azSet := make(map[string]bool)
	for _, n := range nodeList.Items {
		if az, ok := n.Labels["topology.kubernetes.io/zone"]; ok {
			azSet[az] = true
		}
	}
	for az := range azSet {
		azs = append(azs, az)
	}
	sort.Strings(azs)
	if len(azs) == 0 {
		return "", "", nil, fmt.Errorf(
			"no availability zones found in node labels (topology.kubernetes.io/zone)",
		)
	}

	serverID := node.Status.NodeInfo.MachineID
	if serverID == "" {
		return "", "", nil, fmt.Errorf("node %s has no machineID", node.Name)
	}
	vpcID, err = DetectVPCFromECS(d.Credentials, serverID)
	if err != nil {
		return "", "", nil, err
	}

d.detected = &DetectedParams{
VPCID:    vpcID,
SubnetID: subnetID,
AZs:      azs,
}
d.detectedAt = time.Now()
	return vpcID, subnetID, azs, nil
}
