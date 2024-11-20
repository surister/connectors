package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"golang.org/x/sync/errgroup"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// Config represents the fully merged endpoint configuration for Kinesis.
type Config struct {
	Region             string `json:"region" jsonschema:"title=AWS Region,description=The name of the AWS region where the Kinesis stream is located"`
	AWSAccessKeyID     string `json:"awsAccessKeyId" jsonschema:"title=AWS Access Key ID,description=Part of the AWS credentials that will be used to connect to Kinesis"`
	AWSSecretAccessKey string `json:"awsSecretAccessKey" jsonschema:"title=AWS Secret Access Key,description=Part of the AWS credentials that will be used to connect to Kinesis" jsonschema_extras:"secret=true"`

	Advanced advancedConfig `json:"advanced,omitempty"`
}

type advancedConfig struct {
	Endpoint string `json:"endpoint,omitempty" jsonschema:"title=AWS Endpoint,description=The AWS endpoint URI to connect to (useful if you're capturing from a kinesis-compatible API that isn't provided by AWS)"`
}

func (c *Config) Validate() error {
	if c.Region == "" {
		return fmt.Errorf("missing region")
	}
	if c.AWSAccessKeyID == "" {
		return fmt.Errorf("missing awsAccessKeyId")
	}
	if c.AWSSecretAccessKey == "" {
		return fmt.Errorf("missing awsSecretAccessKey")
	}
	return nil
}

func connect(ctx context.Context, cfg *Config) (*kinesis.Client, error) {
	var err = cfg.Validate()
	if err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	opts := []func(*awsConfig.LoadOptions) error{
		awsConfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, ""),
		),
		awsConfig.WithRegion(cfg.Region),
	}

	if cfg.Advanced.Endpoint != "" {
		customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{URL: cfg.Advanced.Endpoint}, nil
		})

		opts = append(opts, awsConfig.WithEndpointResolverWithOptions(customResolver))
	}

	awsCfg, err := awsConfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating aws config: %w", err)
	}

	return kinesis.NewFromConfig(awsCfg), nil
}

type kinesisStream struct {
	name string
	arn  string
}

func listStreams(ctx context.Context, client *kinesis.Client) ([]kinesisStream, error) {
	var out []kinesisStream
	var streamNames []string

	input := &kinesis.ListStreamsInput{}
	for {
		res, err := client.ListStreams(ctx, input)
		if err != nil {
			return nil, err
		}

		streamNames = append(streamNames, res.StreamNames...)

		if !*res.HasMoreStreams {
			break
		}
		input.NextToken = res.NextToken
	}

	// ListStreams only includes stream names and no ARNs, so the
	// DescribeStreamSummary API is used to get the ARN for each individual
	// stream.
	var mu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(5)

	for _, n := range streamNames {
		n := n
		group.Go(func() error {
			desc, err := client.DescribeStreamSummary(groupCtx, &kinesis.DescribeStreamSummaryInput{
				StreamName: &n,
			})
			if err != nil {
				return err
			}

			mu.Lock()
			defer mu.Unlock()

			out = append(out, kinesisStream{
				name: n,
				arn:  *desc.StreamDescriptionSummary.StreamARN,
			})

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	slices.SortFunc(out, func(a, b kinesisStream) int { return cmp.Compare(a.name, b.name) })

	return out, nil
}
