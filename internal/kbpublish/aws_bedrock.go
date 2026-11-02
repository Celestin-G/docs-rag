package kbpublish

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagent"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagent/types"
)

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

func (b *bedrockIngest) Start(ctx context.Context, description string) (Job, error) {
	if len(description) > maxDescriptionLen {
		return Job{}, fmt.Errorf("ingestion job description is %d characters, limit is %d", len(description), maxDescriptionLen)
	}
	out, err := b.client.StartIngestionJob(ctx, &bedrockagent.StartIngestionJobInput{
		KnowledgeBaseId: aws.String(b.kbID),
		DataSourceId:    aws.String(b.dsID),
		Description:     aws.String(description),
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

// Recent returns up to n jobs of the data source, the most recently started
// first, paging as needed.
func (b *bedrockIngest) Recent(ctx context.Context, n int) ([]Job, error) {
	var (
		jobs  []Job
		token *string
	)
	for len(jobs) < n {
		out, err := b.client.ListIngestionJobs(ctx, &bedrockagent.ListIngestionJobsInput{
			KnowledgeBaseId: aws.String(b.kbID),
			DataSourceId:    aws.String(b.dsID),
			MaxResults:      aws.Int32(int32(min(n-len(jobs), 100))),
			NextToken:       token,
			SortBy: &types.IngestionJobSortBy{
				Attribute: types.IngestionJobSortByAttributeStartedAt,
				Order:     types.SortOrderDescending,
			},
		})
		if err != nil {
			return nil, err
		}
		for _, s := range out.IngestionJobSummaries {
			jobs = append(jobs, Job{
				ID:          aws.ToString(s.IngestionJobId),
				Description: aws.ToString(s.Description),
				StartedAt:   aws.ToTime(s.StartedAt),
				Status:      string(s.Status),
				Stats:       statsFrom(s.Statistics),
			})
		}
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	return jobs, nil
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
	return Job{ID: aws.ToString(j.IngestionJobId), Description: aws.ToString(j.Description), StartedAt: aws.ToTime(j.StartedAt), Status: string(j.Status), FailureReasons: j.FailureReasons, Stats: statsFrom(j.Statistics)}
}
