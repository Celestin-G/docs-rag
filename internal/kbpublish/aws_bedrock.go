//go:build bedrockagent

package kbpublish

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagent"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagent/types"
)

// Build with `-tags bedrockagent` once github.com/aws/aws-sdk-go-v2/service/bedrockagent
// is in go.mod. The constraint exists only because that module could not be
// downloaded when this file was written; see aws_nobedrock.go.

// IngestSupported is true: this build can start ingestion jobs.
const IngestSupported = true

type bedrockIngest struct {
	client *bedrockagent.Client
	kbID   string
	dsID   string
}

// NewIngestAPI returns the Bedrock Agent ingestion API for one knowledge base
// and data source.
func NewIngestAPI(cfg aws.Config, kbID, dsID string) IngestAPI {
	return &bedrockIngest{client: bedrockagent.NewFromConfig(cfg), kbID: kbID, dsID: dsID}
}

func (b *bedrockIngest) Start(ctx context.Context) (Job, error) {
	out, err := b.client.StartIngestionJob(ctx, &bedrockagent.StartIngestionJobInput{
		KnowledgeBaseId: aws.String(b.kbID),
		DataSourceId:    aws.String(b.dsID),
	})
	if err != nil {
		var conflict *types.ConflictException
		if errors.As(err, &conflict) {
			return Job{}, ErrIngestConflict
		}
		return Job{}, err
	}
	return jobFrom(out.IngestionJob), nil
}

func (b *bedrockIngest) Get(ctx context.Context, jobID string) (Job, error) {
	out, err := b.client.GetIngestionJob(ctx, &bedrockagent.GetIngestionJobInput{
		KnowledgeBaseId: aws.String(b.kbID),
		DataSourceId:    aws.String(b.dsID),
		IngestionJobId:  aws.String(jobID),
	})
	if err != nil {
		return Job{}, err
	}
	return jobFrom(out.IngestionJob), nil
}

func jobFrom(j *types.IngestionJob) Job {
	if j == nil {
		return Job{}
	}
	job := Job{ID: aws.ToString(j.IngestionJobId), Status: string(j.Status), FailureReasons: j.FailureReasons}
	if s := j.Statistics; s != nil {
		job.Stats = JobStats{
			Scanned:          aws.ToInt64(s.NumberOfDocumentsScanned),
			NewIndexed:       aws.ToInt64(s.NumberOfNewDocumentsIndexed),
			Modified:         aws.ToInt64(s.NumberOfModifiedDocumentsIndexed),
			Deleted:          aws.ToInt64(s.NumberOfDocumentsDeleted),
			Failed:           aws.ToInt64(s.NumberOfDocumentsFailed),
			MetadataScanned:  aws.ToInt64(s.NumberOfMetadataDocumentsScanned),
			MetadataModified: aws.ToInt64(s.NumberOfMetadataDocumentsModified),
		}
	}
	return job
}
