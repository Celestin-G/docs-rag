package kbpublish

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// DefaultRegion is where the knowledge base lives.
const DefaultRegion = "eu-west-1"

// CheckRegion refuses any region outside Europe: the docs go to an EU knowledge
// base, and a stray AWS_REGION must not send them elsewhere.
func CheckRegion(region string) error {
	if !strings.HasPrefix(region, "eu-") {
		return fmt.Errorf("region %q refused: only eu-* regions are allowed", region)
	}
	return nil
}

// LoadAWSConfig loads the default credential chain (AWS_PROFILE, environment,
// SSO, …) for the region. Nothing is read from the repository.
func LoadAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	if err := CheckRegion(region); err != nil {
		return aws.Config{}, err
	}
	return config.LoadDefaultConfig(ctx, config.WithRegion(region))
}

// NewS3Store returns an ObjectStore on the bucket.
func NewS3Store(cfg aws.Config, bucket string) *S3Store {
	return &S3Store{API: s3.NewFromConfig(cfg), Bucket: bucket}
}
