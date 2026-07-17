package huaweicloud_test

import (
	"strings"
	"testing"

	"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

func TestBuildELBName_DefaultPattern(t *testing.T) {
	name := huaweicloud.BuildELBName(nil, "default", "my-svc", "abc123def456")
	// Should be k8s-{ns_8}-{name_8}-{uid_10}
	expected := "k8s-default-my-svc-abc123def4"
	if name != expected {
		t.Errorf("expected %q, got %q", expected, name)
	}
}

func TestBuildELBName_CustomName(t *testing.T) {
	params := map[string]string{
		huaweicloud.LBCNameAnnotation: "my-custom-elb",
	}
	name := huaweicloud.BuildELBName(params, "default", "svc", "abc123def456")
	// Custom name should get a UID suffix appended for uniqueness
	expected := "my-custom-elb-abc123def4"
	if name != expected {
		t.Errorf("expected %q, got %q", expected, name)
	}
}

func TestBuildELBName_TruncatesLongCustomName(t *testing.T) {
	longName := "a-very-very-very-very-very-very-very-very-very-very-very-very-very-very-long-name-that-exceeds-64-chars"
	params := map[string]string{
		huaweicloud.LBCNameAnnotation: longName,
	}
	name := huaweicloud.BuildELBName(params, "ns", "svc", "uid")
	if len([]rune(name)) > 64 {
		t.Errorf("expected name <= 64 chars, got %d chars: %q", len([]rune(name)), name)
	}
	// UID suffix must always be present even after truncation
	uidSuffix := "uid"
	if !strings.HasSuffix(name, "-"+uidSuffix) {
		t.Errorf("expected name to end with UID suffix %q, got %q", "-"+uidSuffix, name)
	}
}

func TestBuildELBName_TruncatesLongNamespace(t *testing.T) {
	longNS := "verylongnamespace-name-that-should-be-truncated"
	name := huaweicloud.BuildELBName(nil, longNS, "svc", "uid1234567")
	// namespace should be truncated to 8 chars
	if len(name) > 64 {
		t.Errorf("expected name <= 64 chars, got %d: %q", len(name), name)
	}
	// Should start with k8s- + first 8 chars of namespace
	expectedPrefix := "k8s-" + longNS[:8]
	if name[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("expected namespace prefix %q in name %q", expectedPrefix, name)
	}
}

func TestIsInternalELB_True(t *testing.T) {
	params := map[string]string{
		huaweicloud.LBCPublicAnnotation: "false",
	}
	if !huaweicloud.IsInternalELB(params) {
		t.Error("expected true for public=false")
	}
	caseInsensitive := map[string]string{
		huaweicloud.LBCPublicAnnotation: "FALSE",
	}
	if !huaweicloud.IsInternalELB(caseInsensitive) {
		t.Error("expected true for public=FALSE (case insensitive)")
	}
}

func TestIsInternalELB_False(t *testing.T) {
	params := map[string]string{
		huaweicloud.LBCPublicAnnotation: "true",
	}
	if huaweicloud.IsInternalELB(params) {
		t.Error("expected false for public=true")
	}

	// Missing key defaults to public (false = not internal)
	if huaweicloud.IsInternalELB(nil) {
		t.Error("expected false for nil params (default public)")
	}
}

func TestBuildCreateELBOption_PublicELB(t *testing.T) {
	params := map[string]string{
		huaweicloud.LBCPublicAnnotation:              "true",
		huaweicloud.LBCBandwidthSizeAnnotation:       "20",
		huaweicloud.LBCBandwidthChargeModeAnnotation: "bandwidth",
		huaweicloud.LBCEIPTypeAnnotation:             "5_sbgp",
	}
	azs := []string{"az1", "az2"}
	opt := huaweicloud.BuildCreateELBOption(params, "vpc-1", "subnet-1", azs, "default", "my-svc", "uid-1234567890")

	if opt.VpcID != "vpc-1" {
		t.Errorf("expected VpcID=vpc-1, got %s", opt.VpcID)
	}
	if opt.VipSubnetCidrID != "subnet-1" {
		t.Errorf("expected VipSubnetCidrID=subnet-1, got %s", opt.VipSubnetCidrID)
	}
	if len(opt.AvailabilityZoneList) != 2 {
		t.Errorf("expected 2 AZs, got %d", len(opt.AvailabilityZoneList))
	}
	if !opt.IsPublic {
		t.Error("expected IsPublic=true")
	}
	if opt.BandwidthSize != 20 {
		t.Errorf("expected BandwidthSize=20, got %d", opt.BandwidthSize)
	}
	if opt.BandwidthChargeMode != "bandwidth" {
		t.Errorf("expected BandwidthChargeMode=bandwidth, got %s", opt.BandwidthChargeMode)
	}
	if opt.PublicIPNetworkType != "5_sbgp" {
		t.Errorf("expected PublicIPNetworkType=5_sbgp, got %s", opt.PublicIPNetworkType)
	}
	// Name should follow the default pattern
	if opt.Name == "" {
		t.Error("expected non-empty Name")
	}
}

func TestBuildCreateELBOption_InternalELB(t *testing.T) {
	params := map[string]string{
		huaweicloud.LBCPublicAnnotation: "false",
	}
	opt := huaweicloud.BuildCreateELBOption(params, "vpc-1", "subnet-1", []string{"az1"}, "ns", "svc", "uid")

	if opt.IsPublic {
		t.Error("expected IsPublic=false for internal ELB")
	}
	// Bandwidth defaults should still apply (even though unused for internal)
	if opt.BandwidthSize != huaweicloud.DefaultBandwidthSize {
		t.Errorf("expected default BandwidthSize=%d, got %d", huaweicloud.DefaultBandwidthSize, opt.BandwidthSize)
	}
}

func TestBuildCreateELBOption_Defaults(t *testing.T) {
	opt := huaweicloud.BuildCreateELBOption(nil, "vpc-1", "subnet-1", nil, "ns", "svc", "uid")

	if !opt.IsPublic {
		t.Error("expected IsPublic=true by default")
	}
	if opt.BandwidthSize != huaweicloud.DefaultBandwidthSize {
		t.Errorf("expected BandwidthSize=%d, got %d", huaweicloud.DefaultBandwidthSize, opt.BandwidthSize)
	}
	if opt.BandwidthChargeMode != huaweicloud.DefaultBandwidthChargeMode {
		t.Errorf("expected BandwidthChargeMode=%s, got %s", huaweicloud.DefaultBandwidthChargeMode, opt.BandwidthChargeMode)
	}
	if opt.PublicIPNetworkType != huaweicloud.DefaultEIPType {
		t.Errorf("expected PublicIPNetworkType=%s, got %s", huaweicloud.DefaultEIPType, opt.PublicIPNetworkType)
	}
}

func TestBuildCreateELBOption_InvalidBandwidth(t *testing.T) {
	params := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "invalid",
	}
	opt := huaweicloud.BuildCreateELBOption(params, "vpc", "subnet", nil, "ns", "svc", "uid")
	if opt.BandwidthSize != huaweicloud.DefaultBandwidthSize {
		t.Errorf("expected default BandwidthSize for invalid input, got %d", opt.BandwidthSize)
	}
}

func TestBuildCreateELBOption_NegativeBandwidth(t *testing.T) {
	params := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "-5",
	}
	opt := huaweicloud.BuildCreateELBOption(params, "vpc", "subnet", nil, "ns", "svc", "uid")
	if opt.BandwidthSize != huaweicloud.DefaultBandwidthSize {
		t.Errorf("expected default BandwidthSize for negative input, got %d", opt.BandwidthSize)
	}
}

func TestBuildCreateELBOption_BandwidthExceedsMax(t *testing.T) {
	params := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "999999",
	}
	opt := huaweicloud.BuildCreateELBOption(params, "vpc", "subnet", nil, "ns", "svc", "uid")
	// Should be clamped to maxBandwidthSize (2000)
	if opt.BandwidthSize != 2000 {
		t.Errorf("expected BandwidthSize=2000 (clamped), got %d", opt.BandwidthSize)
	}
}
