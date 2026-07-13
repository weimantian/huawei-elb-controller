package huaweicloud

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	LBCPublicAnnotation              = "huawei-elb.io/public"
	LBCBandwidthSizeAnnotation       = "huawei-elb.io/bandwidth-size"
	LBCBandwidthChargeModeAnnotation = "huawei-elb.io/bandwidth-charge-mode"
	LBCEIPTypeAnnotation             = "huawei-elb.io/eip-type"
	LBCBandwidthShareTypeAnnotation  = "huawei-elb.io/bandwidth-share-type"
	LBCNameAnnotation                = "huawei-elb.io/name"
)

const CCEAutocreateAnnotation = "kubernetes.io/elb.autocreate"

const (
	DefaultBandwidthSize       = 10
	DefaultBandwidthChargeMode = "traffic"
	DefaultEIPType             = "5_bgp"
	DefaultBandwidthShareType  = "PER"
)

type AutocreateConfig struct {
	Name               string   `json:"name,omitempty"`
	Type               string   `json:"type,omitempty"`
	BandwidthName      string   `json:"bandwidth_name,omitempty"`
	BandwidthChargeMode string  `json:"bandwidth_chargemode,omitempty"`
	BandwidthSize      int32    `json:"bandwidth_size,omitempty"`
	BandwidthShareType string   `json:"bandwidth_sharetype,omitempty"`
	EipType            string   `json:"eip_type,omitempty"`
	VipSubnetCidrID    string   `json:"vip_subnet_cidr_id,omitempty"`
	AvailableZone      []string `json:"available_zone,omitempty"`
}

func BuildAutocreateJSON(lbcParams map[string]string, detectedVPCID, detectedSubnetID string, detectedAZs []string, serviceName string) (string, error) {
	config := BuildAutocreateConfig(lbcParams, detectedVPCID, detectedSubnetID, detectedAZs, serviceName)
	data, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("marshaling autocreate config: %w", err)
	}
	return string(data), nil
}

func BuildAutocreateConfig(lbcParams map[string]string, detectedVPCID, detectedSubnetID string, detectedAZs []string, serviceName string) *AutocreateConfig {
	elbType := resolveELBType(lbcParams)

	cfg := &AutocreateConfig{
		VipSubnetCidrID: detectedSubnetID,
		AvailableZone:   detectedAZs,
	}

	if elbType == "inner" {
		cfg.Name = resolveName(lbcParams, serviceName)
		return cfg
	}

	cfg.Type = elbType
	cfg.Name = resolveName(lbcParams, serviceName)
	cfg.BandwidthName = truncateStr(fmt.Sprintf("cce-lb-%s-bw", serviceName), 64)
	cfg.BandwidthSize = resolveBandwidthSize(lbcParams)
	cfg.BandwidthChargeMode = resolveStringParam(lbcParams, LBCBandwidthChargeModeAnnotation, DefaultBandwidthChargeMode)
	cfg.BandwidthShareType = resolveStringParam(lbcParams, LBCBandwidthShareTypeAnnotation, DefaultBandwidthShareType)
	cfg.EipType = resolveStringParam(lbcParams, LBCEIPTypeAnnotation, DefaultEIPType)

	return cfg
}

func DefaultAutocreateConfig(detectedVPCID, detectedSubnetID string, detectedAZs []string, serviceName string) *AutocreateConfig {
	return BuildAutocreateConfig(nil, detectedVPCID, detectedSubnetID, detectedAZs, serviceName)
}

func resolveELBType(params map[string]string) string {
	if v, ok := params[LBCPublicAnnotation]; ok && strings.ToLower(v) == "false" {
		return "inner"
	}
	return "public"
}

func resolveName(params map[string]string, serviceName string) string {
if v, ok := params[LBCNameAnnotation]; ok && v != "" {
		return truncateStr(v, 64)
}
	return truncateStr(fmt.Sprintf("cce-lb-%s", serviceName), 64)
}

func resolveBandwidthSize(params map[string]string) int32 {
	v, ok := params[LBCBandwidthSizeAnnotation]
	if !ok || v == "" {
		return DefaultBandwidthSize
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n <= 0 {
		return DefaultBandwidthSize
	}
	if n > 2000 {
		return 2000
	}
	return int32(n)
}

func resolveStringParam(params map[string]string, key, defaultVal string) string {
	if v, ok := params[key]; ok && v != "" {
		return v
	}
return defaultVal
}

// truncateStr truncates s to maxLen characters.
func truncateStr(s string, maxLen int) string {
if len(s) <= maxLen {
return s
}
return s[:maxLen]
}
