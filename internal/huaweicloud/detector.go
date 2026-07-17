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
	VPCID     string
	SubnetID  string // VIP subnet (first node's subnet, used for ELB VIP)
	AZs       []string
	SubnetMap map[string]string // virsubnetID -> neutron subnetID (all nodes' subnets)
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
	if err := c.List(ctx, nodeList); err != nil {
		return "", "", nil, fmt.Errorf("listing nodes: %w", err)
	}
	if len(nodeList.Items) == 0 {
		return "", "", nil, fmt.Errorf("no nodes found in cluster")
	}

	node := nodeList.Items[0]

	// Collect all unique virsubnet IDs from nodes (multi-AZ clusters have
	// different subnets per AZ). Each member must use its own subnet.
	virsubnetSet := make(map[string]bool)
	for _, n := range nodeList.Items {
		if vs := n.Labels["node.kubernetes.io/subnetid"]; vs != "" {
			virsubnetSet[vs] = true
		}
	}

	subnetMap := make(map[string]string, len(virsubnetSet))
	for vs := range virsubnetSet {
		neutronID, err := GetNeutronSubnetID(d.Credentials, vs)
		if err != nil {
			return "", "", nil, fmt.Errorf("converting virsubnet %s to neutron subnet: %w", vs, err)
		}
		subnetMap[vs] = neutronID
	}

	// VIP subnet: use the first node's subnet (ELB VIP only needs one subnet).
	firstVirsubnet := node.Labels["node.kubernetes.io/subnetid"]
	if firstVirsubnet == "" {
		return "", "", nil, fmt.Errorf(
			"node %s has no node.kubernetes.io/subnetid label", node.Name)
	}
	subnetID = subnetMap[firstVirsubnet]

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
		VPCID:     vpcID,
		SubnetID:  subnetID,
		AZs:       azs,
		SubnetMap: subnetMap,
	}
	d.detectedAt = time.Now()
	return vpcID, subnetID, azs, nil
}

// GetNeutronSubnet returns the neutron subnet ID for a given virsubnet ID,
// using the cached subnet map from the last Detect() call. If the virsubnet
// is not in the cache, it queries the API and caches the result.
func (d *NetworkDetector) GetNeutronSubnet(virsubnetID string) (string, error) {
	d.detectMu.Lock()
	defer d.detectMu.Unlock()

	if d.detected != nil && time.Since(d.detectedAt) < detectorCacheTTL {
		if id, ok := d.detected.SubnetMap[virsubnetID]; ok {
			return id, nil
		}
	}

	// Cache miss or expired: query API directly.
	neutronID, err := GetNeutronSubnetID(d.Credentials, virsubnetID)
	if err != nil {
		return "", err
	}

	// Update cache map if we have a valid cache.
	if d.detected != nil && d.detected.SubnetMap != nil {
		d.detected.SubnetMap[virsubnetID] = neutronID
	}
	return neutronID, nil
}
