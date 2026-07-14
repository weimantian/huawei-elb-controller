package huaweicloud_test

import (
	"encoding/json"
	"testing"

	"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

func TestBuildAutocreateConfig_PublicELB(t *testing.T) {
	params := map[string]string{
		"huawei-elb.io/public":                "true",
		"huawei-elb.io/bandwidth-size":        "20",
		"huawei-elb.io/bandwidth-charge-mode": "bandwidth",
		"huawei-elb.io/eip-type":              "5_sbgp",
		"huawei-elb.io/bandwidth-share-type":  "WHOLE",
		"huawei-elb.io/name":                  "my-elb",
	}

	cfg := huaweicloud.BuildAutocreateConfig(params, "subnet-456", []string{"az1", "az2"}, "test-service")

	if cfg.Type != "public" {
		t.Errorf("expected Type=public, got %s", cfg.Type)
	}
	if cfg.Name != "my-elb" {
		t.Errorf("expected Name=my-elb, got %s", cfg.Name)
	}
	if cfg.BandwidthSize != 20 {
		t.Errorf("expected BandwidthSize=20, got %d", cfg.BandwidthSize)
	}
	if cfg.BandwidthChargeMode != "bandwidth" {
		t.Errorf("expected BandwidthChargeMode=bandwidth, got %s", cfg.BandwidthChargeMode)
	}
	if cfg.EipType != "5_sbgp" {
		t.Errorf("expected EipType=5_sbgp, got %s", cfg.EipType)
	}
	if cfg.BandwidthShareType != "WHOLE" {
		t.Errorf("expected BandwidthShareType=WHOLE, got %s", cfg.BandwidthShareType)
	}
	if cfg.VipSubnetCidrID != "subnet-456" {
		t.Errorf("expected VipSubnetCidrID=subnet-456, got %s", cfg.VipSubnetCidrID)
	}
	if len(cfg.AvailableZone) != 2 || cfg.AvailableZone[0] != "az1" || cfg.AvailableZone[1] != "az2" {
		t.Errorf("expected AvailableZone=[az1 az2], got %v", cfg.AvailableZone)
	}
}

func TestBuildAutocreateConfig_InnerELB(t *testing.T) {
	params := map[string]string{
		"huawei-elb.io/public": "false",
	}

	cfg := huaweicloud.BuildAutocreateConfig(params, "subnet-456", []string{"az1"}, "test-service")

	if cfg.Type != "" {
		t.Errorf("expected Type empty for inner ELB, got %s", cfg.Type)
	}
	if cfg.BandwidthSize != 0 {
		t.Errorf("expected BandwidthSize=0 for inner ELB, got %d", cfg.BandwidthSize)
	}
	if cfg.VipSubnetCidrID != "subnet-456" {
		t.Errorf("expected VipSubnetCidrID=subnet-456, got %s", cfg.VipSubnetCidrID)
	}
}

func TestBuildAutocreateConfig_Defaults(t *testing.T) {
	cfg := huaweicloud.BuildAutocreateConfig(nil, "subnet-456", nil, "default-svc")

	if cfg.Type != "public" {
		t.Errorf("expected Type=public for empty params, got %s", cfg.Type)
	}
	if cfg.BandwidthSize != 10 {
		t.Errorf("expected BandwidthSize=10 (default), got %d", cfg.BandwidthSize)
	}
	if cfg.BandwidthChargeMode != "traffic" {
		t.Errorf("expected BandwidthChargeMode=traffic (default), got %s", cfg.BandwidthChargeMode)
	}
	if cfg.EipType != "5_bgp" {
		t.Errorf("expected EipType=5_bgp (default), got %s", cfg.EipType)
	}
	if cfg.BandwidthShareType != "PER" {
		t.Errorf("expected BandwidthShareType=PER (default), got %s", cfg.BandwidthShareType)
	}
}

func TestBuildAutocreateJSON(t *testing.T) {
	params := map[string]string{
		"huawei-elb.io/public": "true",
	}

	jsonStr, err := huaweicloud.BuildAutocreateJSON(params, "subnet-1", []string{"az-a"}, "json-test")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &cfg); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}

	if cfg["type"] != "public" {
		t.Errorf("expected type=public in JSON, got %v", cfg["type"])
	}
	if cfg["vip_subnet_cidr_id"] != "subnet-1" {
		t.Errorf("expected vip_subnet_cidr_id=subnet-1, got %v", cfg["vip_subnet_cidr_id"])
	}
}

func TestDefaultAutocreateConfig(t *testing.T) {
	cfg := huaweicloud.DefaultAutocreateConfig("subnet-y", []string{"az-z"}, "default-svc")

	if cfg.Type != "public" {
		t.Errorf("expected Type=public, got %s", cfg.Type)
	}
	if cfg.BandwidthSize != 10 {
		t.Errorf("expected BandwidthSize=10, got %d", cfg.BandwidthSize)
	}
	if cfg.VipSubnetCidrID != "subnet-y" {
		t.Errorf("expected VipSubnetCidrID=subnet-y, got %s", cfg.VipSubnetCidrID)
	}
}
