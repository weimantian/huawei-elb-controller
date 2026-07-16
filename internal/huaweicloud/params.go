package huaweicloud

import (
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
	AnnotationELBID                = "huawei-elb.io/elb-id"
	AnnotationELBCleanupFinalizer  = "huawei-elb.io/elb-cleanup"
)

const (
	DefaultBandwidthSize       = 10
	DefaultBandwidthChargeMode = "traffic"
	DefaultEIPType             = "5_bgp"
	DefaultBandwidthShareType  = "PER"
	maxBandwidthSize           = 2000
	ipGroupListLimit           = 200
)


// BuildELBName generates the ELB name following EKS/GKE naming conventions:
// k8s-{ns_8}-{name_8}-{uid_10}
// Total length ~32 chars, well within Huawei Cloud's 64-char limit.
// If the user specified huawei-elb.io/name, that takes precedence but still
// gets a UID suffix appended to ensure global uniqueness and enable name-based
// reverse-lookup recovery when annotations are overwritten.
func BuildELBName(lbcParams map[string]string, namespace, name, uid string) string {
	uidSuffix := shortStr(uid, 10)
	if v, ok := lbcParams[LBCNameAnnotation]; ok && v != "" {
		// Append UID suffix to custom names too, ensuring global uniqueness so
		// the reverse-lookup recovery path in reconcileCreate works uniformly.
		// Truncate the custom name to leave room for "-{uidSuffix}" (≤11 chars).
		return truncateStr(v, 64-len(uidSuffix)-1) + "-" + uidSuffix
	}
	return truncateStr(fmt.Sprintf("k8s-%s-%s-%s", shortStr(namespace, 8), shortStr(name, 8), uidSuffix), 64)
}

// IsInternalELB returns true if the params indicate an internal (private) ELB.
func IsInternalELB(params map[string]string) bool {
	v, ok := params[LBCPublicAnnotation]
	return ok && strings.ToLower(v) == "false"
}

// BuildCreateELBOption builds the CreateELBOption from LBC params and detected network info.
func BuildCreateELBOption(lbcParams map[string]string, vpcID, subnetID string, azs []string, namespace, name, uid string) CreateELBOption {
	return CreateELBOption{
		Name:                 BuildELBName(lbcParams, namespace, name, uid),
		VpcID:                vpcID,
		VipSubnetCidrID:      subnetID,
		AvailabilityZoneList: azs,
		IsPublic:             !IsInternalELB(lbcParams),
		BandwidthSize:        resolveBandwidthSize(lbcParams),
		BandwidthChargeMode:  resolveStringParam(lbcParams, LBCBandwidthChargeModeAnnotation, DefaultBandwidthChargeMode),
		PublicIPNetworkType:  resolveStringParam(lbcParams, LBCEIPTypeAnnotation, DefaultEIPType),
	}
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
	if n > maxBandwidthSize {
		return maxBandwidthSize
	}
	return int32(n)
}

func resolveStringParam(params map[string]string, key, defaultVal string) string {
	if v, ok := params[key]; ok && v != "" {
		return v
	}
	return defaultVal
}

// truncateStr truncates s to maxLen characters (Unicode-safe).
func truncateStr(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

// shortStr truncates s to maxLen characters.
func shortStr(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}
