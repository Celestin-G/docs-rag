//go:build !bedrockagent

package kbpublish

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// errNoBedrock is returned by the default build. The Bedrock Agent adapter is in
// aws_bedrock.go behind the `bedrockagent` build tag because the
// github.com/aws/aws-sdk-go-v2/service/bedrockagent module could not be
// downloaded in the sandbox this was written in (go get failed TLS verification
// against the filtering proxy), so the adapter has never been compiled. To
// enable it: `go get github.com/aws/aws-sdk-go-v2/service/bedrockagent`, then
// build with `-tags bedrockagent` (or delete both build constraints and this
// file).
// IngestSupported tells the command to refuse a real run before it uploads
// anything it could not then ingest.
const IngestSupported = false

var errNoBedrock = errors.New("this binary was built without the bedrockagent tag: the ingestion job cannot be started (see internal/kbpublish/aws_nobedrock.go)")

type noBedrock struct{}

// NewIngestAPI returns an IngestAPI that fails every call.
func NewIngestAPI(aws.Config, string, string) IngestAPI { return noBedrock{} }

func (noBedrock) Start(context.Context) (Job, error)       { return Job{}, errNoBedrock }
func (noBedrock) Get(context.Context, string) (Job, error) { return Job{}, errNoBedrock }
