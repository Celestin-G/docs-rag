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

// Latest returns the most recently started job of the data source, false when
// the data source has never been synced.
func (b *bedrockIngest) Latest(ctx context.Context) (Job, bool, error) {
	out, err := b.client.ListIngestionJobs(ctx, &bedrockagent.ListIngestionJobsInput{
		KnowledgeBaseId: aws.String(b.kbID),
		DataSourceId:    aws.String(b.dsID),
		MaxResults:      aws.Int32(1),
		SortBy: &types.IngestionJobSortBy{
			Attribute: types.IngestionJobSortByAttributeStartedAt,
			Order:     types.SortOrderDescending,
		},
	})
	if err != nil {
		return Job{}, false, err
	}
	if len(out.IngestionJobSummaries) == 0 {
		return Job{}, false, nil
	}
	s := out.IngestionJobSummaries[0]
	return Job{ID: aws.ToString(s.IngestionJobId), Status: string(s.Status), Stats: statsFrom(s.Statistics)}, true, nil
}

func statsFrom(s *types.IngestionJobStatistics) JobStats {
	if s == nil {
		return JobStats{}
	}
	// The counters are plain int64 in the SDK, not pointers.
	return JobStats{
		Scanned:          s.NumberOfDocumentsScanned,
		NewIndexed:       s.NumberOfNewDocumentsIndexed,
		Modified:         s.NumberOfModifiedDocumentsIndexed,
		Deleted:          s.NumberOfDocumentsDeleted,
		Failed:           s.NumberOfDocumentsFailed,
		MetadataScanned:  s.NumberOfMetadataDocumentsScanned,
		MetadataModified: s.NumberOfMetadataDocumentsModified,
	}
}

func jobFrom(j *types.IngestionJob) Job {
	if j == nil {
		return Job{}
	}
	return Job{ID: aws.ToString(j.IngestionJobId), Status: string(j.Status), FailureReasons: j.FailureReasons, Stats: statsFrom(j.Statistics)}
}
