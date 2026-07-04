// Package huaweicloud provides a client wrapper for Huawei Cloud ELB v3 API.
package huaweicloud

import (
	"fmt"
	"os"

	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/basic"
	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	elbregion "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3/region"
)

// Credentials holds Huawei Cloud authentication credentials.
type Credentials struct {
	AK        string
	SK        string
	Region    string
	ProjectID string
}

// LoadCredentials reads Huawei Cloud credentials from environment variables.
// Required env vars: HUAWEI_CLOUD_AK, HUAWEI_CLOUD_SK,
// HUAWEI_CLOUD_REGION, HUAWEI_CLOUD_PROJECT_ID.
func LoadCredentials() (*Credentials, error) {
	ak := os.Getenv("HUAWEI_CLOUD_AK")
	sk := os.Getenv("HUAWEI_CLOUD_SK")
	regionStr := os.Getenv("HUAWEI_CLOUD_REGION")
	projectID := os.Getenv("HUAWEI_CLOUD_PROJECT_ID")

	if ak == "" || sk == "" || regionStr == "" || projectID == "" {
		return nil, fmt.Errorf(
			"HUAWEI_CLOUD_AK, HUAWEI_CLOUD_SK, HUAWEI_CLOUD_REGION, " +
				"and HUAWEI_CLOUD_PROJECT_ID must be set",
		)
	}

	return &Credentials{
		AK:        ak,
		SK:        sk,
		Region:    regionStr,
		ProjectID: projectID,
	}, nil
}

// NewELBClient creates a Huawei Cloud ELB v3 client from credentials.
func NewELBClient(creds *Credentials) (*elb.ElbClient, error) {
	auth := basic.NewCredentialsBuilder().
		WithAk(creds.AK).
		WithSk(creds.SK).
		WithProjectId(creds.ProjectID).
		Build()

	reg, err := elbregion.SafeValueOf(creds.Region)
	if err != nil {
		return nil, fmt.Errorf("invalid Huawei Cloud region %q: %w", creds.Region, err)
	}

	hcClient, err := elb.ElbClientBuilder().
		WithCredential(auth).
		WithRegion(reg).
		SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("building ELB HTTP client: %w", err)
	}

	return elb.NewElbClient(hcClient), nil
}
