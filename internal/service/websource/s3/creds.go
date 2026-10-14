package s3

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// defaultCredentials resolves the server's AWS credential chain (env, shared
// config, web identity, instance role).
func defaultCredentials(ctx context.Context, region string) (aws.CredentialsProvider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws credentials; %w", err)
	}

	return cfg.Credentials, nil
}
